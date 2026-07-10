# Directory Structure

> 后端目录组织规范。内部项目精简版。

---

## Overview

后端 Go 代码全部位于**仓库根**（无 `src/`、无 `internal/`），模块路径 `github.com/QuantumNous/new-api`（`go.mod`，模块路径与项目标识受 AGENTS.md「Protected project information」保护，禁止改名）。

分层架构（与 AGENTS.md 一致）：**Router → Controller → Service → Model**，外加一个相对独立的转发子系统 **relay/**。硬性编码规范（JSON 包装、三库兼容、行锁、quota 换算、计费不变量）以根目录 `AGENTS.md` 为唯一权威，本文只回答：代码放哪、谁能 import 谁。

---

## Directory Layout

```
main.go            入口：InitResources、embed web/*/dist、启动 Gin
router/            路由注册：api-router.go（管理 API）、relay-router.go（/v1 转发）、
                   dashboard.go、web-router.go（前端静态资源）、video-router.go、authz-router.go
middleware/        auth.go（鉴权）、distributor.go（渠道分发）、限流、CORS、gzip、审计
controller/        HTTP handler。relay.go 是转发入口，按 RelayFormat 分发到 relay 包的 *Helper
service/           业务逻辑：billing.go、quota.go、channel_select.go、通知、支付等
  service/authz/          Casbin 权限（role/permission/enforcer）
  service/passkey/        WebAuthn/Passkey
  service/relayconvert/   Chat ↔ Responses 请求/响应格式互转
model/             GORM 模型 + 全部 DB 访问。main.go 含 dialect helper；locking.go 含 lockForUpdate
relay/             AI 转发子系统
  relay/*.go              各 relay 格式 handler（TextHelper/ImageHelper/ClaudeHelper/GeminiHelper/
                          ResponsesHelper/relay_task.go 等）+ relay_adaptor.go（GetAdaptor 注册表）
  relay/channel/          provider 适配器，每个上游一个子目录（openai/、claude/、gemini/、aws/…）
  relay/channel/task/     任务型平台（视频等）：kling/、sora/、vidu/…，实现 TaskAdaptor
  relay/common/           RelayInfo、计费上下文、请求转换等共享逻辑（可被 service 反向引用）
  relay/helper/           valid_request.go（maxTokensLimit 校验）、price.go、stream_scanner.go
  relay/constant/         relay mode 常量
dto/               请求/响应结构体（openai_request.go、claude.go、gemini.go、task.go…）
types/             跨层类型：NewAPIError、RelayFormat、PriceData（AddOtherRatio）、RequestMeta
constant/          纯常量：channel.go（ChannelType）、api_type.go（APIType）、context_key.go、cache_key.go
setting/           配置管理，按域分子包（system_setting/、operation_setting/、ratio_setting/、
                   model_setting/、billing_setting/…），setting/config/ 是注册中心
common/            基础工具：json.go（Marshal/Unmarshal 强制包装）、quota_math.go（quota 换算唯一入口）、
                   database.go、redis.go、crypto.go、env.go、limiter/
logger/            日志封装
i18n/              后端国际化（go-i18n，en/zh）
oauth/             OAuth provider（github/discord/oidc/linuxdo/generic），registry.go 注册
pkg/               相对独立的内部库：billingexpr/（计费表达式，改前必读 pkg/billingexpr/expr.md）、
                   cachex/、ionet/、perf_metrics/
web/               前端（bun workspace），不属于本文范围
```

---

## Layering & Import Direction

依据实际 import 关系（grep 全仓验证），从底到顶：

| 层 | 包 | 允许 import 的项目内包 |
|---|---|---|
| L0 基础 | `constant` | 无 |
| L0 基础 | `common` | `constant` |
| L0 基础 | `types` | `common` |
| L0 基础 | `logger` | `common`, `setting/operation_setting` |
| L1 数据结构 | `dto` | `common`, `constant`, `logger`, `types` |
| L1 配置 | `setting/*` | `common`, `constant`, `types`, `setting/config`, `pkg/billingexpr` |
| L2 数据 | `model` | L0/L1, `pkg/cachex`（例外：`model/task.go` 引 `relay/common`，属历史现状，不得扩大） |
| L3 业务 | `service` | L0–L2, `relay/common`, `relay/constant`, `relay/helper`, `pkg/*` |
| L3 转发 | `relay` 及 `relay/channel/*` | L0–L2, `service`, `relay/common`, `relay/helper` |
| L4 接入 | `middleware` | L0–L3 |
| L4 接入 | `controller` | L0–L3, `middleware`, `oauth`, `i18n` |
| L5 组装 | `router` | `controller`, `middleware`, `oauth`, `relay`, `service/authz` |
| L6 入口 | `main.go` | 全部 |

关键约束：

- **`relay`（顶层包）import `service`，反向禁止**。service 只能引 `relay/common`、`relay/constant`、`relay/helper` 这类叶子子包，引顶层 `relay` 会成环。
- `model` 不得 import `service`/`controller`/顶层 `relay`。
- 除 `router`/`main.go` 外，任何包不得 import `controller`。
- L0 包（`common`/`constant`/`types`/`logger`）不得 import 任何上层包（`logger` → `setting/operation_setting` 是既有例外，不得新增此类例外）。
- controller 直接调用 `model` 做简单 CRUD 是本项目现状（60 个文件如此），允许；但计费、渠道选择等复杂逻辑必须放 `service`。

---

## Where New Code Goes

- **新增管理 API**：`router/api-router.go` 注册路由 → `controller/<域>.go` 写 handler → 复杂逻辑下沉 `service/`，DB 访问放 `model/`。
- **新增上游 provider（chat 类）**：
  1. `constant/channel.go` 加 `ChannelTypeXxx`；
  2. `constant/api_type.go` 加 `APITypeXxx`，并在 `common/api_type.go` 的 `ChannelType2APIType` 建立映射；
  3. 新建 `relay/channel/<name>/`，实现 `relay/channel/adapter.go` 的 `Adaptor` 接口（惯例文件：`adaptor.go` + `constants.go` + `dto.go` + `relay-<name>.go`，参考 `relay/channel/claude/`）；
  4. `relay/relay_adaptor.go` 的 `GetAdaptor` 加 case；
  5. 确认 provider 是否支持 `StreamOptions`，支持则加入 `streamSupportedChannels`（AGENTS.md）。
- **新增任务型平台（视频/异步任务）**：`relay/channel/task/<platform>/` 实现 `TaskAdaptor`（`relay/channel/adapter.go`），必须实现 `EstimateBilling` / `AdjustBillingOnSubmit` / `AdjustBillingOnComplete` 并遵守 AGENTS.md 计费安全不变量（用户可控倍率必须先设界）。
- **新增请求/响应 DTO**：放 `dto/`。可选标量字段必须 `*int`/`*bool` 等指针 + `omitempty`（AGENTS.md「Preserve explicit zero values」）；max-tokens/count 类字段第一天就在 `relay/helper/valid_request.go` 风格的 validator 中设上界。
- **新增配置组**：在 `setting/` 对应子包定义 struct，`init()` 中注册，模式摘自 `setting/system_setting/passkey.go`：

  ```go
  config.GlobalConfig.Register("passkey", &defaultPasskeySettings)
  ```

- **新增表/字段**：只在 `model/` 改，靠 GORM `AutoMigrate`，必须同时兼容 SQLite/MySQL/PostgreSQL（AGENTS.md）。行锁一律用 `model/locking.go` 的 helper：

  ```go
  // model/locking.go — SQLite 无 FOR UPDATE 语法，helper 内部按 DB 类型分支
  func lockForUpdate(tx *gorm.DB) *gorm.DB {
      if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
          return tx
      }
      return tx.Clauses(clause.Locking{Strength: "UPDATE"})
  }
  ```

- **新增跨包共享类型**：错误/格式/价格类放 `types/`；纯常量放 `constant/`；与具体 relay 请求绑定的结构放 `dto/`。
- **新增独立工具库**：与业务解耦、可单测的放 `pkg/<name>/`；小工具函数放 `common/`。
- **测试**：与被测文件同包同目录（`*_test.go`），新测试必须用 `testify` 的 `require`/`assert`（AGENTS.md）。

---

## Naming Conventions

- 目录即包，包名全小写单词（`relay/channel/baidu_v2` 允许下划线）。
- 文件名用蛇形下划线（`channel_select.go`、`quota_math.go`）。`common/` 下存在历史连字符文件（`rate-limit.go`），新文件一律用下划线。
- provider 适配器目录内：`adaptor.go`（接口实现）、`dto.go`（该 provider 的私有结构）、`constants.go`（模型列表等）、`relay-<name>.go`（收发逻辑）。
- setting 子包按域命名：`<域>_setting/`（如 `billing_setting/`、`perf_metrics_setting/`）。

---

## Forbidden Patterns

- **禁止**业务代码直接调用 `encoding/json` 的 Marshal/Unmarshal，必须走 `common.Marshal` / `common.Unmarshal` / `common.UnmarshalJsonStr` / `common.DecodeJson`（`common/json.go`，AGENTS.md）。`json.RawMessage` 等类型引用不受限。
- **禁止** GORM v1 行锁写法 `tx.Set("gorm:query_option", "FOR UPDATE")`（GORM v2 静默忽略、不加锁），也禁止在调用点自拼 `clause.Locking`；一律用 `model/` 内的 `lockForUpdate(tx)`。
- **禁止** quota/token 换算 bare cast（`int(float64(quota)*ratio)` 之类），一律走 `common/quota_math.go`：`common.QuotaFromFloat` / `common.QuotaRound` / `common.QuotaFromDecimal`（计费路径用 `*Checked` 变体，AGENTS.md）。
- **禁止**单库特性无 fallback：raw SQL 必须处理方言差异（保留字列用 `model/main.go` 的 `commonGroupCol`/`commonKeyCol`，布尔用 `commonTrueVal`/`commonFalseVal`，分支用 `common.UsingMainDatabase(...)`）。
- **禁止**违反 import 方向：`service` → 顶层 `relay`、`model` → `service`、非 `router`/`main` → `controller`、L0 → 上层，均不允许。
- **禁止**在 `relay/channel/<provider>/` 之外堆 provider 私有 DTO；provider 专属结构放该 provider 目录的 `dto.go`。
- **禁止**新增顶层目录承载业务代码；新代码必须归入上表已有目录，确需新顶层包先在团队内对齐。
- **禁止**绕过 `setting/config` 注册机制手写 option 读写；配置组统一 `config.GlobalConfig.Register`。

---

## Examples

- 分发链路示例：`router/relay-router.go` → `middleware/distributor.go` → `controller/relay.go`（按 `types.RelayFormat` 分发）→ `relay/*_handler.go` → `relay/relay_adaptor.go: GetAdaptor(apiType)` → `relay/channel/<provider>/adaptor.go`。
- 结构完整的 provider 适配器参考：`relay/channel/claude/`；任务型参考：`relay/channel/task/kling/adaptor.go`。
- 配置注册参考：`setting/system_setting/passkey.go`；DB 方言处理参考：`model/main.go`、`model/locking.go`。
