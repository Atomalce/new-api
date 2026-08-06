# Design

## Branch and history contract

`main` 是上游镜像，只允许从 `afe16c64` fast-forward 到固定目标
`0ab02020`。`dev` 是 fork 主线，禁止重写。由于 `dev` 已含目标提交，本次不再创建
重复 merge；最终仅在 `dev` 上记录并归档本任务。

## Preservation boundary

上游变更涉及 billing、Responses、RelayKit、前端设置等热点，因此不能只验证 Git
祖先关系。保留性审计覆盖：

- `main.go` 的策略初始化；
- `service/prompt_cache_expiry.go`、`service/billing_usage.go`；
- OpenAI、Gemini 与 Responses 路径的 usage/周期处理；
- billing settings 的后端设置与前端 section 注册；
- fork 的 GHCR workflow 与 Compose override。

## Delivery and rollback

先快进并推送 `main`，再回到 `dev` 验证。若验证失败，不推送新的 `dev` 提交；
`main` 仍是准确的上游镜像，无需回滚。禁止 force-push。
