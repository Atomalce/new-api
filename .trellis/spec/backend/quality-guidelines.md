# Quality Guidelines

> new-api 后端(仓库根 Go 代码)质量规范。内部项目精简版。

---

## Overview

- 适用范围:仓库根的 Go 代码(`main.go`、`common/`、`model/`、`relay/`、`controller/`、`service/`、`middleware/`、`dto/`、`types/`、`constant/`、`setting/`、`router/`、`pkg/`)。前端规范见 `web/default/AGENTS.md`。
- 分层架构:Router → Controller → Service → Model;provider 适配器在 `relay/channel/<provider>/`,实现 `relay/channel/adapter.go` 的 `Adaptor` 接口。
- **权威来源**:根目录 `AGENTS.md` 是上游硬性规范,本文与其一致并做落地引用;若有冲突,以 `AGENTS.md` 为准。
- 数据库必须同时兼容 SQLite / MySQL ≥ 5.7.8 / PostgreSQL ≥ 9.6;Redis 可选。任何 DB 代码不得只在单一方言下正确。

---

## Naming & Package Organization

- Module path:`github.com/QuantumNous/new-api`(受保护标识,禁止改名,见 `AGENTS.md` Project Governance)。
- 包名:全小写、无下划线(如 `billingexpr`、`cachex`)。跨包重名时用固定别名,惯例:`relaycommon "github.com/QuantumNous/new-api/relay/common"`。
- 文件名:snake_case(如 `common/quota_math.go`、`constant/context_key.go`)。`common/`、`controller/` 里存量 kebab-case 文件(`rate-limit.go`、`channel-test.go`)是历史遗留,**新文件禁止 kebab-case**。
- 平台差异文件用 build tag 拆分:`common/system_monitor_unix.go`(`//go:build !windows`)/ `system_monitor_windows.go`。这是仓库中仅有的 build tags。
- Gin context key:统一在 `constant/context_key.go` 用类型化常量(`type ContextKey string`,如 `constant.ContextKeyTokenId`),不得散落字符串字面量。
- Relay 错误:统一用 `*types.NewAPIError`(`types/error.go` 的 `NewError` / `NewErrorWithStatusCode`),`Adaptor.DoResponse` 签名已固定返回该类型。
- 日志:请求内用 `logger.LogInfo/LogWarn/LogError(ctx, ...)`(`logger/logger.go`,带请求关联);系统级/启动期用 `common.SysLog/SysError`(`common/sys_log.go`)。不要 `fmt.Println` / 裸 `log.Print`。
- 函数组织(AGENTS.md Common Code Quality):优先 early return,少嵌套;禁止只有一个调用方、不表达稳定业务概念的包级 helper——直接内联;保留的单用途 helper 命名必须是领域概念而非机械步骤。

---

## Hard Rules (Project-Specific)

以下为 `AGENTS.md` 硬性规则的落地摘要,违反即 review 打回。

### JSON

所有 marshal/unmarshal 必须走 `common/json.go` 包装:`common.Marshal` / `common.Unmarshal` / `common.UnmarshalJsonStr` / `common.DecodeJson` / `common.GetJsonType`。业务代码不得直接调用 `encoding/json`;`json.RawMessage`、`json.Number` 仅可作为类型引用。存量违规(如 `controller/topup_creem.go` 的 `json.Marshal`)是历史债务,不得模仿、顺手可改。

### Database Tri-Compat

- 优先 GORM 方法;主键交给 GORM,禁止手写 `AUTO_INCREMENT` / `SERIAL`。
- 行锁:`model/` 内标准 `SELECT ... FOR UPDATE` 必须用共享 helper `lockForUpdate(tx)`(`model/locking.go`):MySQL/PostgreSQL 发出 `clause.Locking{Strength: "UPDATE"}`,SQLite 跳过。禁止 GORM v1 遗留写法 `tx.Set("gorm:query_option", "FOR UPDATE")`(GORM v2 静默忽略、实际无锁),禁止在调用点重复写 `clause.Locking`。
- 裸 SQL 不可避免时:保留字列用 `model/main.go` 的 `commonGroupCol` / `commonKeyCol`(PG 是 `"group"`,MySQL/SQLite 是 `` `group` ``),布尔用 `commonTrueVal`/`commonFalseVal`;方言分支用 `common.UsingMainDatabase(...)` / `common.UsingLogDatabase(...)`,且每个受支持数据库都要有有效分支。
- 迁移三库可用:SQLite 只能 `ALTER TABLE ... ADD COLUMN`,不支持 `ALTER COLUMN`(模式参考 `model/main.go`)。
- 不加 `gorm:"default:true"` 之类布尔默认 tag(MySQL/PG 规范化差异导致 AutoMigrate 每次重启重复 ALTER),默认值放请求/模型规范化、hook 或 service 逻辑里。

### Quota / Billing

- quota 换算集中在 `common/quota_math.go`,禁止任何本地转换或裸转型:浮点乘积用 `common.QuotaFromFloat`(截断),需四舍五入用 `common.QuotaRound`(half-away-from-zero),decimal 用 `common.QuotaFromDecimal`。饱和上限是 int32(quota 列为 32 位)。
- 计费路径必须用 `*Checked` 变体(`QuotaFromFloatChecked` 等),发生 clamp 时把 `*common.QuotaClamp` 挂到 `relayInfo.QuotaClamp`,写日志前经 `attachQuotaSaturation`(`service/log_info_generate.go`)落到 `other.admin_info.quota_saturation`。
- 一切成为计费乘数的用户可控量必须在校验层设上界并复用既有常量:`dto.MaxImageN`(`dto/openai_image.go`,=128)、`maxTokensLimit`(`relay/helper/valid_request.go`,`math.MaxInt32 / 2`)、`relaycommon.MaxTaskDurationSeconds`。`*uint` 字段仅 `>= 0` 检查不够,必须有上界(超大正数即回绕负数)。
- 倍率写入只走 `types.PriceData.AddOtherRatio`(拒绝非正 / NaN / +Inf),禁止直写 `PriceData.OtherRatios`。
- 新增计费路径要通读全链:validation → EstimateBilling/OtherRatios → quota 换算 → pre-consume → settle/refund,每步保持"永不产生负扣费"不变量。
- 改分层/动态计费表达式前必读 `pkg/billingexpr/expr.md`。

### Relay Request DTOs

- 解析自客户端、再序列化给上游的请求结构,可选标量字段必须"指针 + omitempty",保证显式零值透传、缺省字段省略。真实示例(`dto/openai_request.go:37`):

```go
MaxTokens *uint `json:"max_tokens,omitempty"`
```

- 新增 channel 时确认上游是否支持 `StreamOptions`,支持则加入 `streamSupportedChannels`(`relay/common/relay_info.go`)。

---

## Forbidden Patterns

| 禁止 | 原因 / 替代 |
|---|---|
| 业务代码直接 `json.Marshal` / `json.Unmarshal` / `json.NewDecoder` | 用 `common/json.go` 包装函数 |
| `tx.Set("gorm:query_option", "FOR UPDATE")` | GORM v2 静默忽略,无锁;用 `lockForUpdate(tx)` |
| 调用点手写 `clause.Locking{Strength: "UPDATE"}` | SQLite 不支持该语法;统一走 `lockForUpdate` |
| `int(float64(quota) * ratio)`、`int(math.Round(...))`、`int(d.IntPart())` 等裸转型 | 溢出可产生负扣费;用 `common.QuotaFromFloat/QuotaRound/QuotaFromDecimal` |
| 可选请求参数用非指针标量 + `omitempty` | 显式 `0`/`false` 会被 marshal 静默丢弃;用指针类型 |
| 无跨库分支的方言特性(MySQL-only 函数、PG-only 操作符、SQLite 不支持的 `ALTER COLUMN`、无 TEXT fallback 的 JSON 列型) | 三库必须同时可用 |
| `gorm:"default:true"` 布尔默认 tag | AutoMigrate 重复 ALTER;默认值放代码层 |
| 直写 `PriceData.OtherRatios` map | 绕过非正 / NaN / Inf 守卫;用 `AddOtherRatio` |
| 单调用方、无业务语义的包级 helper;深层嵌套控制流 | 内联 + early return |
| 删除/改名 new-api、QuantumNous 相关标识(module path、品牌、版权等) | 项目治理保护,无例外 |
| 只刷覆盖率的测试、随机输入伪 fuzz、sleep/计时断言、断言私有常量或字段列表 | 见 AGENTS.md Backend test quality |

---

## Lint & Commands

- 仓库未配置 golangci-lint(无 `.golangci.yml`);CI(`.github/workflows/pr-check.yml`)只做 PR 元数据 / anti-slop 检查,**不跑 Go lint 与测试**,质量靠本地自查 + review。
- 本地基线(提交前跑):

```bash
gofmt -l .          # 必须无输出
go vet ./...
go build ./...      # 发布构建等价于 Dockerfile: CGO_ENABLED=0 go build
```

- 本地起后端:`make start-api`(`go run main.go`)或 `make dev-api`(docker compose)。
- 前端(参考):lint 用 oxlint、typecheck 用 tsgo,在 `web/` workspace 内用 bun 执行,详见 `web/default/AGENTS.md`。

---

## Testing

- 运行:仓库根 `go test ./...`。SQLite 驱动是纯 Go 的 `glebarez/sqlite`(底层 modernc),无需 CGO;除 `common/system_monitor_*.go` 的 `windows`/`!windows` 外**没有自定义 build tags**,任何平台直接跑,无需额外 tag 参数。
- 断言库:新增或大改的测试必须用 `github.com/stretchr/testify` —— `require` 做 fixture/致命断言,`assert` 做非致命值断言(见 `model/locking_test.go`)。
- Fixture 惯例:
  - 需要 DB 的测试用内存 SQLite 显式初始化:`gorm.Open(sqlite.Open(":memory:"), ...)`(`model/task_cas_test.go`);
  - 断言生成 SQL 时用 `tests.DummyDialector{}` + `DryRun`,避免驱动吞掉 locking 子句(`model/locking_test.go`);
  - 改了全局状态(如 `common.SetDatabaseTypes`)必须 `t.Cleanup` 恢复;
  - DB、请求上下文、用户组、settings、缓存等状态在测试内显式构造,不依赖执行顺序。
- 写什么:保护真实行为、API 契约、计费/记账不变量、数据兼容、回归路径;优先确定性 table test(显式输入 + 精确期望)。计费边界回归测试放在其保护的边界旁,风格参照 `relay/helper/openai_image_request_test.go`、`relay/common/relay_utils_test.go`、`common/quota_math_test.go`。
- 不写什么:见 Forbidden Patterns 最后一行及 `AGENTS.md` Backend test quality;删除测试时若其间接覆盖了真实契约,须补一个直接断言该契约的更小测试。

---

## Code Review Checklist

- [ ] JSON 操作全部经 `common.Marshal/Unmarshal` 系列,无新增 `encoding/json` 调用
- [ ] DB 变更在 SQLite / MySQL / PostgreSQL 三库均正确;行锁用 `lockForUpdate(tx)`;裸 SQL 有方言分支
- [ ] quota 换算只用 `common/quota_math.go` helper;计费路径用 `*Checked` 并挂 clamp 审计;用户可控乘数有上界
- [ ] 新/改 relay DTO 的可选标量是指针 + `omitempty`,显式零值可透传
- [ ] 新 channel 已确认 `StreamOptions` 支持并按需登记 `streamSupportedChannels`
- [ ] 测试用 testify,状态显式初始化,不属于禁止的测试类型
- [ ] `gofmt` / `go vet` / `go build ./...` 通过
- [ ] 未触碰受保护的项目标识(new-api / QuantumNous)
