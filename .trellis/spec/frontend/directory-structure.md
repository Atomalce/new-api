# Directory Structure

> 前端目录组织规范。内部项目精简版。

---

## Overview

前端是 bun workspace(`web/`),包含两个主题:

- `web/default/` — **主主题**,新功能一律写在这里。React 19 + TypeScript + Rsbuild + TanStack Router/Query/Table + Base UI + Tailwind + Zustand + i18next。
- `web/classic/` — 旧主题,React 18 + Semi Design,JSX 无 TypeScript。仅维护,不作为新功能落点;确需同步 classic 时须任务中明确说明。

行为类规范(i18n、表单、状态管理、错误处理等)以 `web/default/AGENTS.md` 为准,仓库根 `AGENTS.md` 的前端规则(bun 作包管理器、i18n 平铺 JSON 英文原文作 key)为硬性要求。本文只回答一个问题:**代码放哪里**。

常用命令(在 `web/default/` 下执行):`bun run dev` / `bun run build` / `bun run typecheck`(tsgo)/ `bun run lint`(oxlint)/ `bun run i18n:sync`。

---

## Directory Layout — `web/default/src/`

```
src/
├── main.tsx            # 入口:QueryClient、ThemeProvider、RouterProvider 装配
├── routes/             # TanStack Router 文件路由(只做路由声明,不写业务)
├── routeTree.gen.ts    # 路由树,构建时自动生成,禁止手改
├── features/           # 业务模块(一个业务域一个目录),业务代码主体
├── components/         # 跨 feature 复用组件(data-table/、layout/、confirm-dialog.tsx 等)
│   └── ui/             # Base UI 封装的基础组件(button/dialog/…,shadcn 风格,knip 忽略)
├── hooks/              # 通用自定义 hooks,经 hooks/index.ts 桶导出
├── lib/                # 通用工具:api.ts(axios 实例)、format.ts、handle-server-error.ts 等
├── stores/             # Zustand 全局 store:auth-store、notification-store、system-config-store
├── context/            # 全局 Provider:theme / font / direction / layout / search
├── i18n/               # config.ts、static-keys.ts、locales/{en,zh,fr,ru,ja,vi,zh-TW}.json
├── styles/             # 全局 CSS(theme.css、theme-presets.css),组件样式用 Tailwind
├── config/             # 静态配置(fonts.ts)
└── assets/             # 静态资源
```

---

## Feature Modules — `src/features/<feature>/`

一个业务域一个目录(现有:keys、channels、users、dashboard、usage-logs、pricing、playground …)。标准布局以 `web/default/src/features/keys/`、`web/default/src/features/channels/` 为范例:

```
features/<feature>/
├── index.tsx        # 入口组件,供路由 component 引用
├── api.ts           # 该域全部 HTTP 调用(唯一的接口调用落点)
├── types.ts         # 域类型,含 API 请求/响应类型
├── constants.ts     # 枚举/状态/消息常量(值为 i18n key,展示时必须过 t())
├── components/      # 域内组件,可再分 dialogs/、drawers/ 子目录
├── lib/             # 域内纯逻辑、zod/表单 schema(如 channels/lib/channel-form.ts)
└── hooks/           # 域内自定义 hooks(如 channels/hooks/use-channel-mutate-form.ts)
```

- 简单 feature 可以只有 `index.tsx + api.ts + types.ts`;复杂 feature(如 `features/auth/`)可按子流程再分目录(`sign-in/`、`forgot-password/` 各带自己的 `components/`)。
- 只被一个 feature 使用的组件/hook/工具留在该 feature 内;第二个 feature 需要时才上提到 `src/components/`、`src/hooks/`、`src/lib/`。

---

## API Calls

- 全局唯一 axios 实例:`web/default/src/lib/api.ts` 导出 `api`(withCredentials、GET 并发去重、拦截器接 `handleServerError`)。
- 新增接口调用 = 在对应 feature 的 `api.ts` 加导出函数,类型写进同目录 `types.ts`;组件里用 `@tanstack/react-query` 的 `useQuery`/`useMutation` 消费,`queryKey` 用数组形式(如 `['user-groups']`)。
- 真实示例(`web/default/src/features/keys/api.ts`):

```ts
import { api } from '@/lib/api'
import type { GetApiKeysParams, GetApiKeysResponse } from './types'

export async function getApiKeys(
  params: GetApiKeysParams = {}
): Promise<GetApiKeysResponse> {
  const { p = 1, size = 10 } = params
  const res = await api.get(`/api/token/?p=${p}&size=${size}`)
  return res.data
}
```

---

## Routing — `src/routes/`

TanStack Router 文件路由;`routeTree.gen.ts` 由 `@tanstack/router-plugin`(见 `web/default/rsbuild.config.ts` 中 `tanstackRouter`)在 dev/build 时自动生成。

实际目录组织:

```
routes/
├── __root.tsx            # 根布局
├── index.tsx             # /
├── (auth)/               # 无路径分组:sign-in、register、reset…;route.tsx 为组布局
├── (errors)/             # 401/403/404/500/503 错误页
├── _authenticated/       # 需登录的布局路由(route.tsx 做鉴权),业务页挂在下面
│   ├── keys/  channels/  users/  dashboard/  usage-logs/ …
│   └── chat2link.tsx
├── pricing/  about/  rankings/  setup/   # 公开页
├── oauth/$provider.tsx   # 动态段用 $param
└── console/              # 旧路径重定向层(topup.tsx、log.tsx)
```

路由文件只做三件事:`createFileRoute`、`validateSearch`(zod)、`component` 指向 feature 入口,**不写业务实现**。真实示例(`web/default/src/routes/_authenticated/keys/index.tsx`):

```ts
export const Route = createFileRoute('/_authenticated/keys/')({
  validateSearch: apiKeySearchSchema, // zod schema,搜索参数带 .catch() 兜底
  component: ApiKeys,                 // 来自 @/features/keys
})
```

### New Page Checklist

1. `src/features/<feature>/` 建业务模块(入口组件 + `api.ts` + `types.ts`)。
2. `src/routes/` 按 URL 建路由文件,`component` 指向 feature 入口;需登录放 `_authenticated/` 下。
3. 需要侧边栏入口时在 `src/hooks/use-sidebar-data.ts` 注册。
4. 用户可见文案一律 `t('English key')`,然后 `bun run i18n:sync` 同步 locales。
5. 提交前 `bun run typecheck` 与 `bun run lint` 必须通过。

---

## Naming & Import Conventions

- 文件名一律 kebab-case,组件文件也是(如 `api-keys-table.tsx`);导出的组件/类型 PascalCase;hooks 文件 `use-*.ts(x)`。
- 跨目录导入用 `@/` 别名(tsconfig `@/* → src/*`);feature 内部用相对路径(`./types`)。
- 仅类型导入用 `import type`。
- 所有源文件顶部保留 AGPL copyright header(`bun run copyright:check` 校验,`scripts/add-copyright.mjs` 可自动补)。

---

## web/classic

旧主题,结构为传统分层:`src/pages/`(页面)、`src/components/`、`src/helpers/`(API 与工具)、`src/context(s)/`、`src/constants/`、`src/i18n/`。只做缺陷修复类维护,不在此新增功能;目录约定不适用本文上述 feature 规则。

---

## Forbidden Patterns

- **禁止手改 `src/routeTree.gen.ts`** — 自动生成产物(knip 亦忽略),改路由只改 `src/routes/` 下文件。
- **禁止在 `routes/` 下写业务组件或数据逻辑** — 路由文件只做声明,业务进 `features/`。
- **禁止绕过 `src/lib/api.ts`** — 不得自建 axios 实例或裸 `fetch` 调后端;接口调用只落在 feature 的 `api.ts`。
- **禁止硬编码用户可见文案** — 必须 `t('English key')` + 平铺 locale JSON(根 `AGENTS.md` 前端规则)。
- **禁止用 npm/yarn/pnpm** — 前端一律 bun(根 `AGENTS.md`)。
- **禁止直接操作 `window.location` 导航** — 用 `useNavigate` / `Link`(`web/default/AGENTS.md` 3.8)。
- **禁止在 `src/components/ui/` 掺入业务逻辑** — 该目录只放基础 UI 封装。
- **禁止删除或改写 copyright header 及 new-api / QuantumNous 归属信息** — 根 `AGENTS.md` 项目治理保护条款。

---

## References

- 根 `/AGENTS.md` — 前端硬性规则(bun、i18n)、项目治理保护条款。
- `web/default/AGENTS.md` — 详细行为规范(组件、状态、表单、路由行为、测试、构建)。
- 范例模块:`web/default/src/features/keys/`、`web/default/src/features/channels/`。
