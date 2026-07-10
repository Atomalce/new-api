# Logging Guidelines

> new-api 后端日志规范。内部项目精简版。

---

## Overview

本项目**不使用第三方日志库**(无 logrus/zap/zerolog),日志设施为自研两层封装:

| 层 | 位置 | 用途 |
|---|---|---|
| `logger` 包 | `logger/logger.go` | 请求级日志,自动带 request id |
| `common` 包 | `common/sys_log.go` | 系统级日志(启动、DB、定时任务等无请求上下文场景) |

- 输出目标:`gin.DefaultWriter` / `gin.DefaultErrorWriter`,即 stdout/stderr;若启动参数 `--log-dir`(默认 `./logs`,见 `common/init.go`)非空,则 `logger.SetupLogger()` 将其 MultiWriter 到日志文件,并在写满 `maxLogCount`(100 万条)后自动轮转新文件。
- 格式:纯文本行,**非 JSON**:`[LEVEL] 2006/01/02 - 15:04:05 | <request_id> | <msg>`。
- HTTP 访问日志由 `middleware/logger.go` 的 `SetUpLogger` 用 gin formatter 统一输出(`[GIN] time | route_tag | request_id | status | latency | ip | method path`),业务代码不要自己打访问日志。
- 并发安全:writer 的读写受 `common.LogWriterMu`(`common/sys_log.go`)保护,轮转时换 writer 持写锁。业务代码只调用封装函数,**不得**直接向 `gin.DefaultWriter` 写入。

---

## Getting a Logger

没有 logger 实例,直接调用包级函数。

**有请求上下文时** — 用 `logger` 包,第一个参数传 ctx 以携带 request id:

```go
// controller/topup_creem.go
logger.LogError(c.Request.Context(), fmt.Sprintf(
    "Creem 创建充值订单失败 user_id=%d trade_no=%s product_id=%s error=%q",
    id, referenceId, selectedProduct.ProductId, err.Error()))
```

- 传 `*gin.Context`(直接传 `c`)或 `c.Request.Context()` 均可:`middleware/request-id.go` 的 `RequestId()` 同时把 request id 写入了 `c.Set(...)` 和 `c.Request.Context()`,两条路径 `logHelper` 都能取到。现存代码两种写法都有,新代码任选其一即可,但同一文件内保持一致。
- 后台 goroutine/定时任务没有请求 ctx 时传 `context.Background()`,request id 位置会显示 `SYSTEM`(如 `service/system_task.go`)。

**无请求上下文时**(main.go 初始化、model 层 DB 错误回调、缓存同步等) — 用 `common` 包:

```go
// model/log.go
common.SysError("failed to count user logs: " + err.Error())
```

- `common.SysLog(s)` → stdout,`common.SysError(s)` → stderr,前缀 `[SYS]`。
- `common.FatalLog(v...)` 打印后 `os.Exit(1)`,**仅限** main.go / 启动初始化路径使用(如 `main.go:60` "failed to initialize resources"),业务请求处理中禁止调用。

---

## Log Levels

| 函数 | 级别 | 使用场景 |
|---|---|---|
| `logger.LogDebug(ctx, msg, args...)` | DEBUG | 请求体、上游 URL、SSE 心跳等调试细节。仅当环境变量 `DEBUG=true`(`common.DebugEnabled`)时输出;是唯一支持 printf 风格 `args...` 的级别 |
| `logger.LogInfo(ctx, msg)` | INFO | 正常业务里程碑:订单创建成功、consume log 落库、后台任务启动 |
| `logger.LogWarn(ctx, msg)` | WARN | 可恢复异常:退款失败但已记账、缓存降级、quota 饱和钳位(见 AGENTS.md billing 不变量) |
| `logger.LogError(ctx, msg)` | ERR | 请求失败、上游调用失败、数据不一致 |
| `common.FatalLog(v...)` | FATAL | 启动失败,进程退出 |

- 除 `LogDebug` 外各级别只接受已格式化的 string,自己用 `fmt.Sprintf` 拼接。
- 请求体等大对象的调试输出必须走 `LogDebug`(生产环境零输出),不要用 `LogInfo` 打请求体。真实例:`relay/gemini_handler.go:165` `logger.LogDebug(c, "Gemini request body: %s", jsonData)`。
- 调试性打印对象用 `logger.LogJson(ctx, msg, obj)`(内部走 `common.Marshal`,遵守 AGENTS.md 的 JSON 包装规则,且仅 DEBUG 模式输出)。

---

## Structured Fields

日志行不是 JSON,结构化靠 **msg 内的 `key=value` 对**(新代码的既定风格):

```go
// controller/topup_waffo.go
logger.LogInfo(c.Request.Context(), fmt.Sprintf(
    "Waffo 充值订单创建成功 user_id=%d trade_no=%s amount=%d money=%.2f pay_method_type=%s pay_method_name=%q",
    id, merchantOrderId, req.Amount, payMoney, resolvedPayMethodType, resolvedPayMethodName))
```

约定:

- 字段名用 snake_case:`user_id`、`channel_id`、`trade_no`、`task`、`model_name`、`client_ip`、`reason`。
- 可能含空格/特殊字符的字符串值用 `%q`,尤其是 `error=%q`、`path=%q`、`body=%q`。
- 消息开头一句人类可读的动作描述(中英文均有存量,新代码不强制语言),字段跟在其后。
- request id 不需要手写进 msg,`logHelper` 会自动前置;需要在响应/其它系统中引用时用 `c.GetString(common.RequestIdKey)`(键为 `X-Oneapi-Request-Id`,见 `common/constants.go`)。

---

## Sensitive Data

写日志前必须过滤敏感信息,项目已有现成 helper(`common/str.go`),不要自造:

| Helper | 用途 | 真实用例 |
|---|---|---|
| `common.MaskEmail(email)` | 邮箱只留域名(`***@gmail.com`) | `relay/common/relay_info.go:271` |
| `common.MaskSensitiveInfo(str)` | 掩码 URL/IP/域名(渠道 base_url、上游报错里的地址) | `types/error.go:159`、`service/download.go:66` |
| `common.LocalLogPreview(content)` | 大内容截断到 2048 字节(DEBUG 模式不截断) | `controller/relay.go:91`、`service/error.go:96` |

硬性规则:

- **API key / 渠道 key / token key 永远不入日志**。日志里只写 `tokenId`/`channelId` 等 ID;需要标注 key 字段时写死 `***masked***`,参考 `relay/common/relay_info.go`:`Token{ Id: %d, ..., Key: ***masked*** }`。
- 上游错误 message 返回给用户或写日志前经 `types/error.go` 的 `MaskSensitiveInfo` 处理,不要绕过。
- 用户请求体/响应体只允许在 `LogDebug` 或经 `LocalLogPreview` 截断后记录。
- quota 金额展示用 `logger.LogQuota(quota)` / `logger.FormatQuota(quota)`,不要自己按 `QuotaPerUnit` 换算字符串(换算规则集中在 `common/quota_math.go` 与 `logger/logger.go`)。

---

## Forbidden Patterns

- **禁止 `fmt.Println` / `fmt.Printf` 打日志**。存量代码有少量残留(`middleware/rate-limit.go` 等),新代码一律用 `logger.*` / `common.Sys*`;顺手改到相关文件时应替换。唯一例外:`common/init.go` 中 CLI `--help`/`--version` 的面向终端输出。
- **禁止直接使用标准库 `log` 包**(`log.Printf`/`log.Println`)打业务日志,它绕过 request id 和文件轮转。
- **禁止引入第三方日志库**(logrus、zap、slog handler 等),保持与上游 QuantumNous/new-api 的可合并性。
- **禁止直接写 `gin.DefaultWriter` / `gin.DefaultErrorWriter`**,会绕过 `common.LogWriterMu`,轮转时有并发问题。
- **禁止在业务代码调用 `common.FatalLog` / `os.Exit`**,仅限启动路径。
- **禁止把 key、完整邮箱、明文 base_url、未截断请求体写入日志**(见上节)。
- **禁止在日志代码里直接调 `encoding/json`**:序列化对象入日志走 `logger.LogJson` 或 `common.Marshal`(AGENTS.md JSON 规则同样适用于日志路径)。
- **禁止用日志代替错误处理**:不要 log 完就吞掉 error;该向上返回的 error 必须返回,日志只做补充定位。
