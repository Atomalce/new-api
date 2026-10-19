package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func OaiChatToResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(body, &chatResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := chatResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}
	applyUsagePostProcessing(info, &chatResp.Usage, body)

	info.ObserveResponseModel(chatResp.Model)
	if responseID := helper.GetResponseID(c); responseID != "" {
		chatResp.Id = responseID
	}
	convertResult, err := service.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &chatResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	responsesResp, ok := convertResult.Value.(*dto.OpenAIResponsesResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI responses response, got %T", convertResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	usage := upstreamChatUsageSnapshot(&chatResp.Usage)
	if usage == nil || usage.TotalTokens == 0 {
		// locally estimated usage never claims a prompt-cache expiry cycle
		text := service.ExtractOutputTextFromResponses(responsesResp)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		responsesResp.Usage = relayconvert.UsageFromChatUsage(usage)
		service.ApplyPromptCacheDiscountExpiry(c, info, usage, false)
	} else {
		claimEligible := len(chatResp.Choices) > 0 &&
			strings.TrimSpace(chatResp.Choices[0].FinishReason) != "" &&
			service.ResponsesStatusCompleted(responsesResp.Status)
		if service.ApplyPromptCacheDiscountExpiry(c, info, usage, claimEligible) {
			service.ApplyPromptCacheDiscountExpiry(c, info, responsesResp.Usage, false)
		}
	}

	responseBody, err := common.Marshal(responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func OaiChatToResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	responseID := helper.GetResponseID(c)
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, relayconvert.ResponseStreamOptions{
		ID:                 responseID,
		Model:              info.UpstreamModelName,
		EmitSequenceNumber: true,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	streamErr := (*types.NewAPIError)(nil)
	sawTerminalFinishReason := false
	var upstreamUsage *dto.Usage
	// upstreamFailure is the upstream or conversion failure the client was
	// told about with response.failed.
	var upstreamFailure *types.NewAPIError

	sendEvent := func(event relayconvert.ChatToResponsesStreamEvent) bool {
		data, err := common.Marshal(event.Payload)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: event.Type}, string(data)); err != nil {
			// When the client went away the upstream already produced output,
			// so the stream stops and settles normally instead of failing; any
			// other write error stays a server error.
			if c.Request.Context().Err() != nil {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
				return false
			}
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
		return true
	}
	failResponsesStream := func(err error) bool {
		failureResults, handled := state.FailResponsesStream("server_error", err.Error(), "")
		if !handled {
			return false
		}
		for _, result := range failureResults {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				streamErr = types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return true
			}
			if !sendEvent(event) {
				return true
			}
		}
		return true
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			stopStream(sr, streamErr)
			return
		}

		var errorResp dto.OpenAITextResponse
		if err := common.UnmarshalJsonStr(data, &errorResp); err == nil {
			if oaiError := errorResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
				if failResponsesStream(fmt.Errorf("%s", oaiError.Message)) {
					// The client got response.failed, so the request is never
					// retried; the recorded error keeps the stream failed.
					upstreamFailure = types.WithOpenAIError(*oaiError, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
					sr.Stop(upstreamFailure)
					return
				}
				streamErr = types.WithOpenAIError(*oaiError, resp.StatusCode)
				stopStream(sr, streamErr)
				return
			}
		}

		var chunk dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
			logger.LogError(c, common.LogText("failed to unmarshal chat stream response: %s", err.Error()))
			if failResponsesStream(err) {
				upstreamFailure = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
				sr.Stop(upstreamFailure)
				return
			}
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			stopStream(sr, streamErr)
			return
		}
		if chunk.Usage != nil {
			applyUsagePostProcessing(info, chunk.Usage, common.StringToByteSlice(data))
			if snapshot := upstreamChatUsageSnapshot(chunk.Usage); snapshot != nil {
				if upstreamUsage == nil {
					upstreamUsage = snapshot
				} else {
					// usage may be split across chunks; keep every non-zero field
					upstreamUsage = dto.MergeUsageNonZero(upstreamUsage, snapshot)
				}
			}
		}
		if chunk.IsFinished() {
			sawTerminalFinishReason = true
		}

		info.ObserveResponseModel(chunk.Model)
		results, err := service.ConvertStreamResponseChunk(c, info, state, &chunk)
		if err != nil {
			if failResponsesStream(err) {
				upstreamFailure = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
				sr.Stop(upstreamFailure)
				return
			}
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			stopStream(sr, streamErr)
			return
		}
		for _, result := range results {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				streamErr = types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				stopStream(sr, streamErr)
				return
			}
			if !sendEvent(event) {
				stopStream(sr, streamErr)
				return
			}
		}
	})

	if streamErr != nil {
		return nil, streamErr
	}

	usage := upstreamUsage
	realUpstreamUsage := usage != nil && usage.TotalTokens != 0
	convertedUsage := state.Usage()
	if upstreamFailure != nil && !realUpstreamUsage && (convertedUsage == nil || convertedUsage.TotalTokens == 0) && state.UsageText() == "" {
		// The stream failed before any output reached the client, which
		// already received response.failed: the request is not charged.
		return nil, upstreamFailure
	}
	if !realUpstreamUsage {
		// locally estimated usage never claims a prompt-cache expiry cycle
		usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		state.SetUsage(usage)
	} else {
		// response.completed reports the settled usage, including counts
		// merged from several chunks; a copy keeps the stream state from
		// rewriting the settled usage
		visibleUsage := *usage
		state.SetUsage(&visibleUsage)
	}
	if info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone {
		return usage, nil
	}

	finalResults, err := service.FinalizeStreamResponse(c, info, state)
	if err != nil {
		if failResponsesStream(err) {
			return usage, streamErr
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	claimEligible := realUpstreamUsage && sawTerminalFinishReason && len(finalResults) > 0
	if claimEligible {
		lastEvent, ok := finalResults[len(finalResults)-1].Value.(relayconvert.ChatToResponsesStreamEvent)
		claimEligible = ok && lastEvent.Type == "response.completed"
	}
	if service.ApplyPromptCacheDiscountExpiry(c, info, usage, claimEligible) {
		for i := range finalResults {
			event, ok := finalResults[i].Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				continue
			}
			if event.Payload.Response != nil && event.Payload.Response.Usage != nil {
				service.ApplyPromptCacheDiscountExpiry(c, info, event.Payload.Response.Usage, false)
				finalResults[i].Value = event
			}
		}
	}
	for _, result := range finalResults {
		event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
		if !ok {
			return nil, types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
		if !sendEvent(event) {
			if streamErr != nil {
				return nil, streamErr
			}
			return usage, nil
		}
	}

	return usage, nil
}

// upstreamChatUsageSnapshot copies the usage an upstream Chat Completions
// response reported, or returns nil when it carries no token counts at all.
// Like the Responses conversion, a missing total_tokens is rebuilt from the
// prompt and completion counts, so such usage is billed as reported rather
// than replaced by a local estimate. A snapshot whose total is still zero
// (for example cache counts only) is kept for stream merging but is not real
// usage on its own.
func upstreamChatUsageSnapshot(src *dto.Usage) *dto.Usage {
	if !dto.HasOpenAIUsageTokens(src) {
		return nil
	}
	snapshot := *src
	if snapshot.TotalTokens == 0 {
		snapshot.TotalTokens = snapshot.PromptTokens + snapshot.CompletionTokens
	}
	if src.InputTokensDetails != nil {
		details := *src.InputTokensDetails
		snapshot.InputTokensDetails = &details
	}
	snapshot.BillingUsage = dto.CloneBillingUsage(src.BillingUsage)
	if snapshot.BillingUsage == nil {
		snapshot.BillingUsage = dto.NewOpenAIChatBillingUsage(&snapshot)
	}
	return &snapshot
}

// stopStream ends a converted stream from the scanner callback. A nil
// streamErr (the stream stops because the client went away) must reach
// StreamResult.Stop as an untyped nil; a typed nil *NewAPIError is a non-nil
// error that StreamResult would record as a failure and dereference.
func stopStream(sr *helper.StreamResult, streamErr *types.NewAPIError) {
	if streamErr == nil {
		sr.Stop(nil)
		return
	}
	sr.Stop(streamErr)
}
