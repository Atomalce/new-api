# Implementation Plan

1. 再次确认工作区、远端和固定目标，验证 `main` 可 fast-forward。
2. 切换 `main`，执行 `git merge --ff-only upstream/main` 并推送 `origin/main`。
3. 回到 `dev`，确认固定目标已是祖先，不创建重复 merge。
4. 审计 Prompt Cache Expiry Billing、GHCR 与 Compose 的当前接线。
5. 运行根模块、RelayKit、前端及针对性功能测试；检查冲突标记和 whitespace。
6. 提交并归档 Trellis 任务，记录 journal，推送 `origin/dev`。
7. 核对本地/远端 SHA、分歧为 `0 0`，并用远端对象验证关键文件。
