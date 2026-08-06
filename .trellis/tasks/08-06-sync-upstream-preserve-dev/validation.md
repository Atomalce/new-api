# Validation Record

## Git and preservation

- `main`、`origin/main`、`upstream/main` 均为
  `0ab02020603d22e5613bc4cf46bfab06f8567769`；`main` 通过 `--ff-only` 更新并已推送。
- `dev` 已通过既有合并提交 `12237e5e` 包含固定上游目标，因此未创建重复 merge。
- `0ab02020`、`b50eb27a`、`8ab050ed`、`87ccf5f9` 均验证为 `dev` 祖先。
- Prompt Cache Expiry Billing 的后端策略、usage 处理、Relay 状态、管理设置和前端
  section 注册仍在；`main.go` 仍调用 `service.InitPromptCacheDiscountExpiry()`。
- `.github/workflows/ghcr-dev.yml` 与 `docker-compose.override.yml` 在本地 `dev` 及
  同步前的 `origin/dev` 中均存在。
- `git diff --check` 通过，未发现 `<<<<<<<` / `>>>>>>>` 冲突标记。

## Backend and RelayKit

- `GOWORK=off make test`：通过，包含根模块与 RelayKit 测试。
- `GOWORK=off go build ./...`：通过。
- `GOWORK=off go test -race ./service ./relay/channel/openai ./relay/channel/gemini
  -run 'PromptCacheExpiry|InitPromptCacheDiscountExpiry|ValidatePromptCacheExpiry'`：通过。

## Frontend

- `bun run typecheck`：通过。
- `bun run build`：通过。
- 对 Prompt Cache 设置、billing section、OAuth callback 与 API key cell 运行
  `oxlint`：0 issues。
- 全量 `bun test` 未全绿：3 个 API key ring 断言失败，另有 6 个 Bun 对
  `node:test` 的兼容性未处理错误。所有诊断涉及的测试/源码文件均逐个验证与
  固定上游 `main` blob 完全一致，不是本次 fork 回归。
- 全量 `format:check` 与 `copyright:check` 分别被
  `api-key-group-cell.tsx`、`oauth-callback-mode.ts` 的固定上游基线阻断；两文件
  均验证与 `main` blob 完全一致。

## Scope limits

- 未启动线上服务，未连接生产数据库或 Redis，未发起真实供应商请求。
- 最终 `dev == origin/dev` 与干净工作区在任务归档、journal 提交和最后一次推送后核对。
