package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"
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

	if responseID := helper.GetResponseID(c); responseID != "" {
		chatResp.Id = responseID
	}
	convertResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &chatResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	responsesResp, ok := convertResult.Value.(*dto.OpenAIResponsesResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI responses response, got %T", convertResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	canonicalUsage := chatResp.Usage
	if chatResp.Usage.InputTokensDetails != nil {
		details := *chatResp.Usage.InputTokensDetails
		canonicalUsage.InputTokensDetails = &details
	}
	canonicalUsage.BillingUsage = dto.CloneBillingUsage(chatResp.Usage.BillingUsage)
	if canonicalUsage.BillingUsage == nil {
		canonicalUsage.BillingUsage = dto.NewOpenAIChatBillingUsage(&chatResp.Usage)
	}
	usage := &canonicalUsage
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
		ID:    responseID,
		Model: info.UpstreamModelName,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	streamErr := (*types.NewAPIError)(nil)
	sawTerminalFinishReason := false
	var upstreamUsage *dto.Usage

	sendEvent := func(event relayconvert.ChatToResponsesStreamEvent) bool {
		data, err := common.Marshal(event.Payload)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: event.Type}, string(data))
		return true
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			sr.Stop(streamErr)
			return
		}

		var errorResp dto.OpenAITextResponse
		if err := common.UnmarshalJsonStr(data, &errorResp); err == nil {
			if oaiError := errorResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
				streamErr = types.WithOpenAIError(*oaiError, resp.StatusCode)
				sr.Stop(streamErr)
				return
			}
		}

		var chunk dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
			logger.LogError(c, "failed to unmarshal chat stream response: "+err.Error())
			sr.Error(err)
			return
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens != 0 {
			applyUsagePostProcessing(info, chunk.Usage, common.StringToByteSlice(data))
			canonicalUsage := *chunk.Usage
			if chunk.Usage.InputTokensDetails != nil {
				details := *chunk.Usage.InputTokensDetails
				canonicalUsage.InputTokensDetails = &details
			}
			canonicalUsage.BillingUsage = dto.CloneBillingUsage(chunk.Usage.BillingUsage)
			if canonicalUsage.BillingUsage == nil {
				canonicalUsage.BillingUsage = dto.NewOpenAIChatBillingUsage(chunk.Usage)
			}
			upstreamUsage = &canonicalUsage
		}
		if chunk.IsFinished() {
			sawTerminalFinishReason = true
		}

		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, &chunk)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
		for _, result := range results {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				streamErr = types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
			}
			if !sendEvent(event) {
				sr.Stop(streamErr)
				return
			}
		}
	})

	if streamErr != nil {
		return nil, streamErr
	}

	usage := upstreamUsage
	realUpstreamUsage := usage != nil && usage.TotalTokens != 0
	if !realUpstreamUsage {
		// locally estimated usage never claims a prompt-cache expiry cycle
		usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		state.SetUsage(usage)
	}

	finalResults, err := relayconvert.FinalizeStreamResponse(c, info, state)
	if err != nil {
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
			return nil, streamErr
		}
	}

	return usage, nil
}
