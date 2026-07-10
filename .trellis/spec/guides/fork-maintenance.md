# Fork Maintenance Guide

> Fork 二次开发与上游同步规范。内部项目精简版。

---

## Overview

本仓库是 [QuantumNous/new-api](https://github.com/QuantumNous/new-api) 的 fork(origin = Atomalce/new-api),做二次开发并持续合并上游 main。本文档约定分支模型、上游同步流程、冲突处理规则与部署方式。**仓库根 `AGENTS.md` 的上游硬性规范(JSON 包装函数、三库兼容、行锁、quota 换算、品牌保护)优先级最高,本文不重复、只补充 fork 侧规则。**

---

## Branch Model

| 分支 | 用途 | 规则 |
|------|------|------|
| `main` | 上游镜像 | **只接受 `--ff-only` 合并 upstream/main,禁止直接提交** |
| `dev` | 二开主线 | 所有自研提交在此;定期 merge main 吸收上游 |
| `feat/*` | 大功能可选 | 从 dev 切出,完成后合回 dev |

```bash
# 上游同步(标准流程)
git fetch upstream
git checkout main && git merge --ff-only upstream/main && git push origin main
git checkout dev  && git merge main    # 冲突只在这一步解决
```

---

## Conflict Playbook

| 文件 | 规则 |
|------|------|
| `VERSION` | 仓库里是**空文件**,版本由 CI 构建时写入;**自己永远不往里写内容**,冲突取上游 |
| `web/bun.lock` | 取上游后在 `web/` 重跑 `bun install`,提交再生成的锁文件 |
| `web/default/src/routeTree.gen.ts` | TanStack Router 生成文件;不手解,合并后跑一次 `bun run dev` 或 `bun run build` 自动重生成 |
| `web/default/src/i18n/locales/*.json` | flat JSON、英文原文作 key;逐 key 合并,自研文案与上游文案通常可共存 |
| `go.mod` / `go.sum` | 手解依赖行后 `go mod tidy` |
| `README*.md` | 上游更新频繁;自研说明放独立文档,README 冲突取上游 |

合并后必跑:`go build ./... && cd web && bun run typecheck && bun run lint`。

---

## Fork-specific Hard Rules

- **品牌与署名受 AGPLv3 附加条款保护**:不改名、不删 new-api / QuantumNous 标识、不移除 UI 里的原项目链接——这也直接缩小合并冲突面。
- **自研代码尽量放独立文件/目录**,新后端路由前缀记得同步 `web/default/rsbuild.config.ts` 与 `web/classic/rsbuild.config.ts` 的 devProxy(现只代理 `/api` `/mj` `/pg`)。
- **禁用上游 CI**:fork 上的 `docker-image-*.yml`、`release.yml` 没配 secrets 会跑失败,GitHub 设置里保持 Disabled;给上游提 PR 时注意 `pr-check.yml` 的 anti-slop 检查(封锁 "Generated with Claude Code" 字样)与 PR 模板。
- **格式化只用 `bun run format`**(带版权头保护脚本),禁止裸 prettier。

---

## Deployment (云服务器)

链路:**push dev → GitHub Actions 构建镜像 → ghcr.io/atomalce/new-api → 云服务器 pull**。

- 服务器用仓库 `docker-compose.yml`,image 从 `calciumion/new-api:latest` 改为 GHCR 自有镜像。
- 生产必须设 `SQL_DSN`(PG/MySQL,compose 模板已含 postgres);SQLite 仅限本地开发。多实例部署才需要 `SESSION_SECRET`。
- 服务器更新:`docker compose pull && docker compose up -d`。

---

## Local Development Quick Reference

```bash
# 首次:造 go:embed 占位 dist(dist 不入库,没有它编译直接失败)
mkdir -p web/default/dist web/classic/dist
echo '<!doctype html><html><body>dev</body></html>' | tee web/default/dist/index.html web/classic/dist/index.html

# 后端 :3000(零外部依赖:SQLite + 内存缓存;首次进 setup 向导)
go run main.go

# 前端 :5173(HMR,代理 /api /mj /pg → :3000;bun install 必须在 web/ 执行)
cd web && bun install && make -C .. dev-web
```

`make reset-setup` 可重置首次安装向导状态。完整产物:`make build-all-web && go build -o new-api`。

---

## Forbidden Patterns

- ❌ 在 `main` 上直接提交任何自研代码
- ❌ 往 `VERSION` 文件写内容
- ❌ 手工编辑 `routeTree.gen.ts` 解冲突(必须重新生成)
- ❌ 违反根 `AGENTS.md` 硬性规范(common.Marshal / 三库兼容 / lockForUpdate / quota_math)
- ❌ `git push --force` 到 `main`
