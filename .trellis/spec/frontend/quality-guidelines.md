# Frontend Quality Guidelines

> 前端代码质量规范。内部项目精简版。

---

## Overview

前端位于 `web/`(bun workspace,见 `web/package.json` 的 `workspaces: ["default", "classic"]`):

- `web/default/` — 主主题:React 19 函数组件 + TypeScript + Rsbuild + TanStack Router + Base UI + Tailwind。本文档主要约束该目录。
- `web/classic/` — 旧主题:React 18 + Semi Design,仅维护性修改。

本文档与仓库根 `AGENTS.md`(Frontend Rules 一节)及 `web/default/AGENTS.md` 保持一致;冲突时以这两份文件为准。后端规范(`common.Marshal`、DB 三库兼容、`lockForUpdate(tx)`、`common/quota_math.go` 等)见根 `AGENTS.md` 与 `.trellis/spec/backend/`,不在本文范围。

包管理与脚本运行统一用 **bun**(根 `AGENTS.md` 硬性要求),禁止 npm/yarn/pnpm。共享依赖版本走 workspace catalog(`web/package.json` 的 `catalog` 字段),新增依赖优先引用 `"catalog:"`。

---

## Toolchain & Commands

以下命令均在 `web/default/` 下执行(脚本定义见 `web/default/package.json`):

| 用途 | 命令 | 实际执行 |
| --- | --- | --- |
| Lint | `bun run lint` / `bun run lint:fix` | `oxlint -c .oxlintrc.json .` |
| Typecheck | `bun run typecheck` | `tsgo -b`(`@typescript/native-preview`) |
| Format | `bun run format` / `format:check` | `scripts/format-with-protected-headers.mjs` → `oxfmt -c .oxfmtrc.json` |
| 版权头 | `bun run copyright` / `copyright:check` | `scripts/add-copyright.mjs` |
| i18n 对齐 | `bun run i18n:sync` | `scripts/sync-i18n.mjs` |
| 死代码/依赖 | `bun run knip` | `knip`(配置 `knip.config.ts`) |
| 构建 + 类型 | `bun run build:check` | `tsgo -b && rsbuild build` |

关键 lint 规则(摘自 `web/default/.oxlintrc.json`):`categories.correctness: "error"`、`no-nested-ternary: error`、`import/no-cycle: error`、`eqeqeq: error`(null 除外)、`no-console: warn`、`no-unused-vars: warn`(`^_` 前缀豁免)。`ignorePatterns` 中排除了 `src/components/ui` 与 `src/routeTree.gen.ts`,不要试图对它们跑 lint 修复。

格式化必须走 `bun run format`,不要直接裸跑 `oxfmt` —— 包装脚本负责保护文件头的 QuantumNous AGPL 版权注释不被格式化破坏。

---

## Pre-commit Checklist

无 git hooks,提交前手动执行(`web/default/` 下):

```bash
bun run typecheck      # 必须 0 error(AGENTS.md 硬性要求)
bun run lint           # 所涉及文件 lint error 必须清零;warning 按风险评估
bun run format:check
bun run copyright:check  # 新增 .ts/.tsx/.js 等文件必须带版权头,缺失时跑 bun run copyright
bun run i18n:sync      # 仅当改动了文案或 locales
```

大改动或动了构建/路由配置时,追加 `bun run build:check`。`web/default/AGENTS.md` §3.2 的两条硬性规则:每次改动 TS/TSX 后必须 typecheck 修复至无错;完成改动前必须修复所涉文件的所有 lint error,不得遗留。

---

## i18n Workflow

翻译文件:`web/default/src/i18n/locales/{lang}.json`(en、zh、zh-TW、fr、ru、ja、vi),flat JSON,包在 `translation` 命名空间下,**key 即英文原文**。运行时配置见 `src/i18n/config.ts`(`fallbackLng: 'en'`、`nsSeparator: false` 所以 key 中允许冒号)。

添加新文案的流程:

1. 组件内用 `useTranslation()`,以英文原文作 key 调用 `t()`。真实示例(`web/default/src/features/auth/components/oauth-callback-screen.tsx`):

   ```tsx
   const { t } = useTranslation()
   // ...
   t('Signing you in with {{provider}}', { provider: providerLabel })
   ```

   子组件即使父组件已调用 `useTranslation()` 也要自行调用,保证语言切换时重渲染。非 React 环境(工具函数/常量)可 `import { t } from 'i18next'`,但不会随语言切换更新。

2. 在 `locales/en.json` 与 `locales/zh.json` 中同时登记该 key(en 中 key = value):

   ```json
   "Add Announcement": "添加公告"
   ```

   (摘自 `zh.json`;en.json 对应 `"Add Announcement": "Add Announcement"`。)

3. 运行 `bun run i18n:sync`:按 key 最多的 locale 作基准对齐所有语言文件的 key 顺序,缺失 key 用英文回填,未翻译项写入 `locales/_reports/{lang}.untranslated.json`,多余 key 移入 `locales/_extras/`。这些产物是脚本生成的报告,不要手改。

4. **动态 key**(常量/配置中的 label,不以 `t('...')` 字面量出现在代码里的)必须登记到 `src/i18n/static-keys.ts` 的 `STATIC_I18N_KEYS`,否则扫描工具会漏掉。

5. 品牌词与不需翻译的字面量(OpenAI、GitHub 等)登记在 `scripts/sync-i18n.mjs` 的 `BRAND_AND_LITERAL_KEYS`,避免被误报为未翻译。

6. 消息常量(如各 feature `constants.ts` 中的 `SUCCESS_MESSAGES`/`ERROR_MESSAGES`)的值只是 i18n key,展示时必须包 `t()`:`toast.success(t(SUCCESS_MESSAGES.API_KEY_CREATED))`。

`web/classic/` 的 i18n 工具链不同:用 `i18next-cli`(`bun run i18n:extract` / `i18n:status` / `i18n:sync` / `i18n:lint`,见 `web/classic/package.json`),不要混用两套流程。

---

## Forbidden Patterns

- **手改生成文件**:`src/routeTree.gen.ts` 由 `@tanstack/router-plugin`(注册于 `rsbuild.config.ts`)自动生成,dev/build 时会被覆盖;`src/i18n/locales/_reports/`、`_extras/` 由 `i18n:sync` 生成。lint 与 knip 配置均已排除它们,任何手工编辑都会丢失。
- **删除或改写文件头版权注释**:所有 JS/TS 源文件头部的 QuantumNous AGPL 头由 `scripts/add-copyright.mjs` 维护,且根 `AGENTS.md` Project Governance 明确保护 new-api / QuantumNous 的品牌与归属信息,不得移除或替换。
- **硬编码用户可见文案**:任何面向用户的字符串不经 `t()` 直接写死(包括直接 `toast.success(SUCCESS_MESSAGES.xxx)` 当最终文案)。
- **两层及以上嵌套三元表达式**:`no-nested-ternary` 为 error,改用 if-else / 提前返回 / 抽函数。
- **`any` 滥用**:优先具体类型或 `unknown`;仅类型用途的导入必须 `import type { X } from '...'`。
- **直接操作 `window.location` 导航**:路由跳转用 TanStack Router 的 `useNavigate` / `Link`。
- **用 npm/yarn/pnpm 安装依赖**:会破坏 bun workspace 的 lockfile(`web/bun.lock`)与 catalog 解析。
- **在 `src/components/ui/` 中写业务逻辑**:该目录是基础 UI 组件层,lint 与 knip 均已排除;业务组件放 `src/features/<feature>/components/` 或 `src/components/`。
- **提交遗留 `console.log`**:`no-console` 为 warn,提交前清理调试输出。
- **对组件 props 做非必要解构**:直接 `props.xxx` 访问(`web/default/AGENTS.md` §3.2/§3.3)。

---

## Required Patterns

- React 函数组件 + Hooks,单一职责;props 有明确类型;单文件超约 200 行考虑拆分子组件或抽自定义 Hook(Hook 规范见 `.trellis/spec/frontend/hook-guidelines.md`)。
- 数据获取用 `@tanstack/react-query`(`useQuery`/`useMutation` + 数组形式 `queryKey`);全局状态用 Zustand 且组件内用选择器订阅;表单用 React Hook Form + Zod。
- 样式以 Tailwind 工具类为主,动态类名用 `cn()` 合并;避免内联样式。
- 服务端错误统一走 `handleServerError`,提示用 `toast.error` 且文案过 i18n。

---

## Testing Requirements

摘自 `web/default/AGENTS.md` §3.14(vitest 为可选依赖,当前无强制覆盖率门禁):

- 工具函数与纯逻辑优先 Vitest 单测(`*.test.ts`);组件用 React Testing Library 测行为,不测实现细节。
- 测试必须保护真实用户行为、稳定 API 契约或明确回归路径;禁止为覆盖率添加 smoke/sleep/随机输入/日志断言类测试。
- 优先使用 Vitest 与 RTL 的标准断言,只有表达项目特定业务不变量时才抽测试 helper。

---

## Code Review Checklist

- [ ] `bun run typecheck`、`bun run lint`、`bun run format:check` 全部通过
- [ ] 新文件带版权头(`bun run copyright:check`)
- [ ] 新增用户可见文案全部过 `t()`,en/zh locale 已登记,动态 key 已进 `static-keys.ts`,跑过 `i18n:sync`
- [ ] 未触碰 `routeTree.gen.ts`、`locales/_reports/`、`locales/_extras/`
- [ ] 无嵌套三元、无 `any`、无遗留 `console.log`、无直接 `window.location` 导航
- [ ] 依赖变更走 bun 且优先 catalog 版本
