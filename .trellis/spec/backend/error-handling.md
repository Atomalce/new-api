# Error Handling

> new-api 后端错误处理规范。内部项目精简版。

---

## Overview

后端有两条完全不同的错误通道,不得混用:

| 通道 | 错误类型 | HTTP 响应形态 | 适用范围 |
|------|---------|--------------|---------|
| **Relay 通道**(转发 LLM 请求) | `*types.NewAPIError` | OpenAI/Claude 兼容错误体 + 真实 HTTP 状态码 | `relay/`、`controller/relay*.go`、`middleware/` 鉴权与限流 |
| **Dashboard 通道**(管理后台 API) | 普通 `error` | 恒为 HTTP 200,`{"success": false, "message": "..."}` | `controller/` 中的用户/渠道/日志等管理接口 |

另有两类特殊格式:异步任务用 `*dto.TaskError`(`dto/task.go`),Midjourney 代理用 `*dto.MidjourneyResponse`(`dto/midjourney.go`)。

核心类型与错误码定义集中在 `types/error.go`;上游错误解析在 `service/error.go`;Dashboard 响应帮助函数在 `common/gin.go`。

---

## Error Types & Codes

### NewAPIError(`types/error.go`)

Relay 链路的统一错误类型。字段全部通过构造函数与 Options 设置,不要手工拼 struct:

```go
// types/error.go
type NewAPIError struct {
    Err            error           // 底层错误
    RelayError     any             // 原始上游错误体(OpenAIError / ClaudeError)
    skipRetry      bool            // 是否跳过渠道重试
    recordErrorLog *bool           // 是否写错误日志(默认 true)
    errorType      ErrorType       // new_api_error / openai_error / claude_error ...
    errorCode      ErrorCode       // 见下
    StatusCode     int
    Metadata       json.RawMessage
}
```

实现了 `Unwrap()`,支持 `errors.Is` / `errors.As`。

### ErrorCode

错误码是 `types.ErrorCode` 字符串常量,统一定义在 `types/error.go`,按前缀分组。新增错误码必须加在这里,不得在业务代码里写裸字符串。真实示例:

- 请求类:`invalid_request`、`read_request_body_failed`、`convert_request_failed`、`access_denied`、`sensitive_words_detected`
- 内部类:`count_token_failed`、`model_price_error`、`json_marshal_failed`、`do_request_failed`、`get_channel_failed`
- 渠道类(带 `channel:` 前缀,**该前缀有语义**,`types.IsChannelError` 据此判定必然重试+可触发自动禁用):`channel:no_available_key`、`channel:invalid_key`、`channel:model_mapped_error`
- 响应类:`bad_response_status_code`、`bad_response_body`、`empty_response`、`model_not_found`
- 配额类:`insufficient_user_quota`、`pre_consume_token_quota_failed`
- DB 类:`query_data_error`、`update_data_error`

### ErrorType

`types.ErrorType` 标记错误体的原始协议:`new_api_error`(本站产生)、`openai_error` / `claude_error` / `gemini_error`(上游透传)、`upstream_error` 等。由构造函数自动设置,不要手动改。

---

## Wrapping & Propagation

### 构造函数(唯一入口)

全部在 `types/error.go`:

```go
types.NewError(err, types.ErrorCodeInvalidRequest)                    // 默认 500
types.NewErrorWithStatusCode(err, code, http.StatusBadRequest)       // 指定状态码
types.NewOpenAIError(err, code, statusCode)                          // 包装为 OpenAI 形态
types.WithOpenAIError(openAIError, statusCode)                       // 直接采纳上游 OpenAI 错误体
types.WithClaudeError(claudeError, statusCode)                       // 直接采纳上游 Claude 错误体
```

`NewError` / `NewOpenAIError` 内部用 `errors.As` 检测入参是否已是 `*NewAPIError`,是则原样透传(仅追加 Options)。因此**深层已包装的错误直接向上 return,不要二次包装**——重复包装不会报错,但会丢失外层意图。

### Options

行为标记通过 Options 传入,不要直接改私有字段:

```go
// controller/relay.go:113 真实用例
types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed,
    http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
```

- `ErrOptionWithSkipRetry()` — 本错误不应触发渠道重试(客户端问题、体积超限等)
- `ErrOptionWithStatusCode(code)` — 覆盖状态码
- `ErrOptionWithNoRecordErrorLog()` — 不写入错误日志表
- `ErrOptionWithHideErrMsg(str)` — 对客户端隐藏原始错误消息

### 传递链

Relay 全链路的函数签名统一返回 `*types.NewAPIError`(非普通 `error`),例如 adaptor 接口:

```go
// relay/channel/adapter.go:27
DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError)
```

上游 HTTP 非 2xx 响应统一交给 `service.RelayErrorHandler(ctx, resp, showBodyWhenFail)`(`service/error.go:86`)解析:它用 `dto.GeneralErrorResponse`(`dto/error.go`)兼容各家上游的 error/message/msg/detail 等字段形态,能提取 OpenAI 结构则 `types.WithOpenAIError` 保留原始错误体,否则回退 `bad_response_status_code`。**不要自己在 adaptor 里手写上游错误体解析**,除非该 provider 格式确实无法被 `GeneralErrorResponse` 覆盖。

`model/` 层返回普通 `error`(如 `model/user.go` 的 `errors.New("id 为空！")`),由 controller/service 决定包装为 `NewAPIError`(relay 链路)或直接交给 `common.ApiError`(dashboard 链路)。

---

## HTTP Response Conversion

### Relay 通道:controller/relay.go 的 defer 统一出口

`Relay()` 用 defer 兜底,按请求协议格式转换错误体,业务代码只需给 `newAPIError` 赋值后 return:

```go
// controller/relay.go:89
defer func() {
    if newAPIError != nil {
        logger.LogError(c, fmt.Sprintf("relay error: %s", common.LocalLogPreview(newAPIError.Error())))
        newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
        switch relayFormat {
        case types.RelayFormatOpenAIRealtime:
            helper.WssError(c, ws, newAPIError.ToOpenAIError())
        case types.RelayFormatClaude:
            c.JSON(newAPIError.StatusCode, gin.H{"type": "error", "error": newAPIError.ToClaudeError()})
        default:
            c.JSON(newAPIError.StatusCode, gin.H{"error": newAPIError.ToOpenAIError()})
        }
    }
}()
```

要点:

- 错误消息统一追加 request id(`common.MessageWithRequestId`,`common/utils.go:293`)。
- `ToOpenAIError()` / `ToClaudeError()` 内部调用 `common.MaskSensitiveInfo`(`common/str.go`)脱敏,防止上游 key、内网地址泄漏给客户端。新增出口必须走这两个方法,不得直接序列化 `Err.Error()`。
- 渠道可配置状态码映射,出口前经 `service.ResetStatusCode`(`service/error.go:133`)。

### 中间件层

middleware 内拒绝请求统一用 `abortWithOpenAiMessage`(`middleware/utils.go:12`):输出 OpenAI 兼容错误体(`type: "new_api_error"`)、附加 request id、`c.Abort()` 并写日志。可选传 `types.ErrorCode`(如 `middleware/auth.go:399` 传 `types.ErrorCodeAccessDenied`)。

### Dashboard 通道:common/gin.go

管理接口**永远返回 HTTP 200**,用 body 里的 `success` 字段表达成败(前端契约,不得改为真实状态码):

```go
// common/gin.go:199
func ApiError(c *gin.Context, err error) {
    c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
}
```

可用:`common.ApiError(c, err)` / `common.ApiErrorMsg(c, msg)` / `common.ApiSuccess(c, data)`,需要 i18n 时用 `common.ApiErrorI18n(c, key, args...)`。不要在 controller 里手写 `c.JSON(200, gin.H{"success": ...})`。

### Task / Midjourney 通道

- 异步任务(Suno/视频等)用 `service.TaskErrorWrapper` / `TaskErrorWrapperLocal`(`service/error.go:187`)产出 `*dto.TaskError`;`LocalError=true` 表示本地错误(不计入上游失败)。`NewAPIError` 转 `TaskError` 用 `service.TaskErrorFromAPIError`。
- Midjourney 用 `service.MidjourneyErrorWrapper(code, desc)`,响应体为 `{code, description, ...}`。

---

## Retry & Channel Disable Semantics

错误对象直接驱动重试与渠道自动禁用,构造错误时必须考虑这两个语义(`controller/relay.go:325` `shouldRetry`、`service/channel.go:45` `ShouldDisableChannel`):

- `channel:` 前缀错误码 → 必然重试、可触发自动禁用(渠道自身问题)。
- `ErrOptionWithSkipRetry()` → 不重试、不触发禁用(客户端问题)。给 4xx 客户端错误加上它,否则会浪费重试次数把请求打到其他渠道。
- 其余按状态码走 `operation_setting.ShouldRetryByStatusCode` / `ShouldDisableByStatusCode` 配置。
- 错误日志入库由 `types.IsRecordErrorLog` 控制(`controller/relay.go:367`),记录 `error_type`、`error_code`、`status_code`、渠道信息。

---

## Logging

- 请求上下文内:`logger.LogError(ctx, msg)` / `LogWarn` / `LogInfo`(`logger/logger.go`),自动携带 request id。
- 无请求上下文(启动、后台任务、quota 饱和审计):`common.SysError(msg)`(`common/sys_log.go`)。
- 日志中的长错误体用 `common.LocalLogPreview` 截断(`common/str.go:27`),对外消息用 `common.MaskSensitiveInfo` 脱敏。

---

## Forbidden Patterns

- **禁止**在 relay 链路返回普通 `error` 或自造错误 struct——签名必须是 `*types.NewAPIError`,经 `types/error.go` 的构造函数产生。
- **禁止**在业务代码写裸错误码字符串;错误码常量集中在 `types/error.go`,`channel:` 前缀只能用于"渠道自身故障"语义。
- **禁止**绕过 `ToOpenAIError()` / `ToClaudeError()` / `abortWithOpenAiMessage` 直接把 `err.Error()` 写进响应体——会绕过脱敏(`MaskSensitiveInfo`)与 request id 注入。
- **禁止** Dashboard 接口返回非 200 状态码或自拼 `success/message` JSON,统一用 `common/gin.go` 的 `ApiError/ApiSuccess` 系列。
- **禁止**对已是 `*NewAPIError` 的错误二次 `types.NewError` 包装换码;直接透传,需要补充行为时用 Options。
- **禁止**手动构造 `NewAPIError{...}` struct 字面量(私有字段会缺省,errorType/errorCode 失效)。
- **禁止**在错误路径直接 `json.Marshal` 错误体——一切 JSON 序列化按 AGENTS.md 走 `common.Marshal/Unmarshal`(`common/json.go`),`service/error.go` 解析上游错误体即为范例。
- **禁止**吞掉配额相关错误:预扣费失败必须以 `insufficient_user_quota`/`pre_consume_token_quota_failed` 上抛,退款逻辑依赖 defer 中的 `newAPIError != nil` 判定(`controller/relay.go:170`),静默吞错会导致漏退款。

---

## References

- `types/error.go` — NewAPIError、ErrorType/ErrorCode 全量定义、构造函数与 Options
- `service/error.go` — 上游错误解析(RelayErrorHandler)、Task/Midjourney 包装、状态码映射
- `dto/error.go` — GeneralErrorResponse(上游错误体兼容解析)
- `controller/relay.go` — 错误出口 defer、shouldRetry、processChannelError
- `common/gin.go` — Dashboard ApiError/ApiSuccess;`middleware/utils.go` — abortWithOpenAiMessage
- 根目录 `AGENTS.md` — JSON 包装、DB 三库兼容、quota 换算(`common/quota_math.go`)等上游硬性规范,错误处理代码同样受其约束
