# 同步上游并保留 dev 功能

## Goal

将 `upstream/main` 的固定目标 `0ab02020603d22e5613bc4cf46bfab06f8567769`
安全同步到 fork：`main` 保持纯上游镜像，`dev` 保留全部二次开发历史与功能。

## Background

- `main`、`origin/main` 当前为 `afe16c64`，可快进 13 个提交到目标。
- `dev`、`origin/dev` 当前为 `87ccf5f9`，已通过合并提交 `12237e5e`
  包含该上游目标，之后还有 fork 提交 `87ccf5f9`。
- 工作区在创建本任务前干净，不允许重写或覆盖 `dev` 历史。

## Requirements

- 仅使用 fast-forward 更新 `main`，不得在 `main` 上生成 fork 提交。
- 不执行 rebase、force-push、reset 或 whole-file 覆盖式处理。
- 若 `dev` 已包含固定上游目标，不制造无意义的重复 merge；只提交任务生命周期记录。
- 保留并验证 Codex Responses Prompt Cache Expiry Billing 的启动、usage 归一化、
  周期认领、管理设置和全 Responses 路径接入。
- 保留 `.github/workflows/ghcr-dev.yml` 与 `docker-compose.override.yml` 的 fork 部署契约。
- 推送前运行与 fork 功能及当前上游变更相称的后端、RelayKit、前端验证。

## Acceptance Criteria

- [x] `main == origin/main == upstream/main == 0ab02020...`，且更新为 fast-forward。
- [x] `0ab02020...` 与 fork 功能提交均为最终 `dev` 的祖先。
- [x] Prompt Cache Expiry Billing 的关键文件、调用链和针对性测试仍存在并通过。
- [x] GHCR workflow、Compose override 在本地与同步前的 `origin/dev` 中均存在。
- [ ] 最终交付后 `dev == origin/dev`、工作区干净、无冲突标记、`git diff --check` 通过。
- [x] 未触碰线上服务、生产数据库、Redis 或真实供应商请求。

## Validation Record

详见 `validation.md`。最终远端 SHA 对齐是任务归档与 journal 提交完成后的交付门禁，
需在最后一次 `git push origin dev` 后现场核对。
