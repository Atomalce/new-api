# State Management

> new-api 前端状态管理规范。内部项目精简版。

---

## Overview

主题前端 `web/default/` 的状态分五类,各有唯一归属机制,不得混用:

| 状态类别 | 方案 | 位置 |
|---------|------|------|
| **服务端状态**(后端 API 数据) | TanStack Query(`@tanstack/react-query` v5) | 各 feature 内 `useQuery`/`useMutation` + `features/<feature>/api.ts` |
| **全局客户端状态**(登录用户、系统配置、通知已读) | Zustand v5 | `web/default/src/stores/` |
| **应用级 UI 环境**(主题、字体、文字方向、布局、搜索面板) | React Context Provider | `web/default/src/context/` |
| **Feature 局部 UI 状态**(弹窗开关、当前行、刷新触发) | feature 内 Context + `useState` | `features/<feature>/components/<feature>-provider.tsx` |
| **URL 状态**(表格分页/筛选) | TanStack Router search params | `hooks/use-table-url-state.ts` |

旧主题 `web/classic/` 使用 Context + `useReducer`(`web/classic/src/context/{User,Status,Theme}`),无 zustand/react-query。classic 只做维护,不引入新的状态方案。

判断归属的原则:**来自后端的数据一律是服务端状态,交给 React Query 缓存,不要复制进 Zustand 或 useState**(唯一例外见下文 system-config 同步)。会话内跨页面共享且与后端无关的才进 Zustand;单页面/单弹窗生命周期的用局部 state;需要可分享/可刷新恢复的(分页、筛选)进 URL。

---

## Server State: TanStack Query

### 全局 QueryClient(`web/default/src/main.tsx`)

QueryClient 在 `main.tsx` 创建并注入 Router context(`createRouter({ context: { queryClient } })`)。全局默认值已定,业务代码不要在单个 query 上重复配置这些行为:

- `retry`:DEV 不重试;PROD 最多 3 次;401/403 永不重试。
- `refetchOnWindowFocus: false`(防止日志等重页面在切换标签时静默重跑)。
- `staleTime: 10 * 1000`。
- `QueryCache.onError` 统一处理 401(toast + `useAuthStore.getState().auth.reset()` + 跳转 `/sign-in`)和 500(跳转 `/500`)。
- mutation 全局 `onError` 走 `handleServerError`(`lib/handle-server-error.ts`)。

### queryKey 约定

数组形式,首元素为资源名,后接影响结果的全部参数。真实示例(`features/users/components/users-table.tsx:89`):

```ts
const { data, isLoading, isFetching } = useQuery({
  queryKey: ['users', pagination.pageIndex + 1, pagination.pageSize,
    globalFilter, statusFilter, roleFilter, groupFilter, refreshTrigger],
  queryFn: async () => { ... },
  placeholderData: (previousData) => previousData,
})
```

- 分页/筛选参数必须全部进 queryKey,漏一个就会读到脏缓存。
- 分页表格用 `placeholderData: (prev) => prev` 保留上一页数据防闪烁。
- feature provider 的 `refreshTrigger` 计数器可作为 queryKey 成员实现整表刷新(见"Feature-local State")。

### queryFn 与 Dashboard API 契约

后端管理接口**恒返回 HTTP 200**,以 body 的 `success` 字段表达成败(见 `.trellis/spec/backend/error-handling.md`)。因此 axios 层不会为业务失败抛错,**queryFn 内必须检查 `result.success`**,失败时 toast 并返回安全的空值,而不是抛异常:

```ts
// features/users/components/users-table.tsx queryFn 内
if (!result.success) {
  toast.error(result.message || `Failed to ...`)
  return { items: [], total: 0 }
}
```

请求函数统一放在 feature 的 `api.ts`,使用 `lib/api.ts` 的共享 axios 实例 `api`,返回类型标注为 feature `types.ts` 里的 `ApiResponse<T>` 系形状(如 `features/users/api.ts` 的 `getUsers`/`searchUsers`)。组件里不要直接 `axios.get`。

### Mutations 与缓存失效

变更用 `useMutation`,成功后 `invalidateQueries` 相关 queryKey。真实示例(`features/system-settings/hooks/use-update-option.ts`):

```ts
return useMutation({
  mutationFn: (request: UpdateOptionRequest) => updateSystemOption(request),
  onSuccess: (data, variables) => {
    if (data.success) {
      queryClient.invalidateQueries({ queryKey: ['system-options'] })
      if (STATUS_RELATED_KEYS.includes(variables.key)) {
        queryClient.invalidateQueries({ queryKey: ['status'] })
      }
      toast.success(i18next.t('Setting updated successfully'))
    } else {
      toast.error(data.message || i18next.t('Failed to update setting'))
    }
  },
})
```

注意 `onSuccess` 里仍要分支 `data.success`(HTTP 200 契约)。用户可见文案一律 `t('English key')` / `i18next.t(...)`(根 AGENTS.md i18n 规则,flat JSON、英文原文作 key)。

---

## Global Client State: Zustand (`src/stores/`)

现有三个 store,新增全局状态先确认无法归入它们:

- `auth-store.ts` — 登录用户(`useAuthStore`),初始化时从 localStorage `user` 恢复,`setUser`/`reset` 内手动读写 localStorage。
- `system-config-store.ts` — 站点名/logo/货币配置(`useSystemConfigStore`),用 `persist` 中间件 + `partialize`。
- `notification-store.ts` — 公告已读状态,同样 `persist` + `partialize`。

约定:

- `create<State>()(...)` 显式类型化 state 与 actions;actions 定义在 store 内,组件不得直接 `setState` 拼对象。
- **持久化优先用 `persist` 中间件并 `partialize` 只落必要字段**(`system-config-store.ts:96`);`auth-store.ts` 的手动 localStorage 写法是历史存量,新 store 不要模仿。
- 组件内**用选择器订阅**,避免整 store 订阅引发多余渲染(`web/default/AGENTS.md` 3.5):

```ts
// hooks/use-sidebar-view.ts:50
const userRole = useAuthStore((s) => s.auth.user?.role)
```

- React 之外(路由回调、QueryCache 回调、queryFn)用 `useXxxStore.getState()`,如 `main.tsx:90` 的 `useAuthStore.getState().auth.reset()`。
- 一个文件一个 store,职责单一,文件名 `<domain>-store.ts`。

### 唯一的 server→store 同步例外

`/api/status` 是启动关键路径(品牌、货币显示),采用 cache-first:`hooks/use-status.ts` 的 queryFn 在拉取后同时 `useSystemConfigStore.getState().setConfig(mapStatusDataToConfig(status))` 并写 localStorage `status`。这是**有意为之的既有例外**,不要以它为先例把其他服务端数据复制进 store。

---

## App-level Context Providers (`src/context/`)

主题/字体/方向等横切 UI 环境用 Context Provider,在 `main.tsx` 组合:`QueryClientProvider > ThemeProvider > FontProvider > DirectionProvider > RouterProvider`。模式以 `theme-provider.tsx` 为准:

- state 用 `useState` + 惰性初始化(从 cookie/localStorage 恢复,如 `getStoredTheme`)。
- setter 用 `useCallback` 包裹并同步持久化(`setCookie`)。
- 导出 `useXxx()` 消费 hook;在 Provider 外调用时抛错(feature provider 同此约定)。

新增此类 Provider 的门槛:必须是真正全局、与后端数据无关、且被多个不相干模块消费的 UI 环境。否则用 feature provider 或 Zustand。

---

## Feature-local State: `<feature>-provider.tsx`

每个 CRUD feature 的弹窗/抽屉编排状态放在 feature 内的 Context Provider(users/channels/keys/models/usage-logs 等均已采用)。标准形状(`features/users/components/users-provider.tsx`):

```tsx
export function UsersProvider({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useDialogState<UsersDialogType>(null)   // hooks/use-dialog.ts
  const [currentRow, setCurrentRow] = useState<User | null>(null)
  const [refreshTrigger, setRefreshTrigger] = useState(0)
  const triggerRefresh = () => setRefreshTrigger((prev) => prev + 1)
  return <UsersContext value={{ open, setOpen, currentRow, setCurrentRow, refreshTrigger, triggerRefresh }}>{children}</UsersContext>
}

export const useUsers = () => {
  const usersContext = React.useContext(UsersContext)
  if (!usersContext) throw new Error('useUsers has to be used within <UsersContext>')
  return usersContext
}
```

- 固定三件套:`open`(当前弹窗类型,用 `useDialogState`)、`currentRow`(被操作行)、`refreshTrigger`/`triggerRefresh`(计入表格 queryKey,弹窗提交成功后调用以刷新列表)。
- Provider 只在该 feature 的入口组件包裹(`features/users/index.tsx`),不上提到全局。
- 新 feature 的列表页照抄此结构,不要发明新的弹窗状态管理方式。

---

## URL State

表格分页、全局搜索、列筛选通过 TanStack Router 的 search params 管理,统一走 `hooks/use-table-url-state.ts`(接收 route 的 `search` + `navigate`,内部映射为 `PaginationState`/`ColumnFiltersState`)。page size 持久化在 localStorage `page-size`(与 classic 主题共享同一 key,不得改名)。路由 search params 用 Zod schema + `validateSearch` 校验(`web/default/AGENTS.md` 3.8)。不要用 `useState` 私藏分页状态导致刷新丢失。

---

## Forbidden Patterns

- **禁止**用 `useEffect` + `useState` 手写数据拉取——服务端数据一律 `useQuery`;变更一律 `useMutation` + `invalidateQueries`。
- **禁止**把服务端数据复制进 Zustand store 或组件 state 作为"缓存"(唯一既有例外:`use-status.ts` 同步 system config)。
- **禁止**绕过 feature `api.ts` 在组件里直接调 axios,或不用 `lib/api.ts` 的共享 `api` 实例。
- **禁止**在 queryFn/onSuccess 中忽略 `success` 字段——Dashboard API 恒 200,不检查 `result.success` 会把业务失败当成功渲染。
- **禁止**queryKey 遗漏影响结果的参数,或用字符串拼接代替数组形式。
- **禁止**整 store 订阅(`const store = useXxxStore()`)后只用其中一个字段——用选择器。存量代码中 `const { auth } = useAuthStore()` 属待清理写法,新代码不得新增。
- **禁止**新增全局 Context 来传业务数据(Context 只用于 UI 环境与 feature 弹窗编排);也禁止为 feature 局部状态新建 Zustand store。
- **禁止**在 store 之外的业务代码直接读写 localStorage 管理状态——持久化收敛在 store(`persist`)、theme cookie、`use-table-url-state` 的 `page-size` 等既有入口。
- **禁止**在 classic 主题引入 zustand/react-query 或新的状态库;classic 维持 Context + `useReducer`。
- **禁止**硬编码用户可见提示文案——toast/错误消息必须 `t('English key')`(根 AGENTS.md i18n 规则)。

---

## References

- `web/default/src/main.tsx` — QueryClient 全局配置、Provider 组合、router context
- `web/default/src/stores/` — auth-store / system-config-store / notification-store(Zustand 全量示例)
- `web/default/src/features/users/` — api.ts + users-provider.tsx + users-table.tsx(feature 状态组织范式)
- `web/default/src/hooks/use-status.ts`、`use-table-url-state.ts` — server→store 同步例外、URL 状态
- `web/default/AGENTS.md` §3.5/3.6/3.8 — 状态管理、API 请求、路由细则;根 `AGENTS.md` — i18n 与 bun 等硬性规范
- 同目录 `hook-guidelines.md` — 自定义 hooks 规范(状态相关 hook 的命名与拆分)
