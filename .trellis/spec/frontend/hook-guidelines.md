# Hook Guidelines

> new-api 前端自定义 Hooks 规范。内部项目精简版。

---

## Overview

前端为 React 19 函数组件,逻辑复用统一走自定义 hooks(无 class 组件、无 HOC 新增)。两套主题的现状:

| 主题 | 技术栈 | hooks 位置与命名 |
|------|--------|-----------------|
| `web/default/`(主) | React 19 + TS + TanStack Query/Router + Zustand | kebab-case 文件 `use-*.ts`,命名导出 `useXxx` |
| `web/classic/`(旧) | React 18 + Vite + Semi Design,JS 为主 | `src/hooks/<domain>/useXxx.js(x)`,camelCase 文件 |

新功能一律写在 `web/default/`;改动 `web/classic/` 时遵循其既有 camelCase 约定,不要把两套风格互相搬运。本文其余部分均针对 `web/default/`。

详细的组件/状态/请求规范见 `web/default/AGENTS.md`(3.3 组件、3.5 状态管理、3.6 API 请求);仓库根 `AGENTS.md` 的前端条目(bun、i18n flat JSON、英文原文作 key)与"Protected project information"条款同样约束 hooks 代码。

---

## Location

按复用范围三级放置,不要放错层:

| 位置 | 用途 | 现有示例 |
|------|------|---------|
| `src/hooks/` | 跨 feature 的通用 hooks | `use-debounce.ts`、`use-dialog.ts`、`use-mobile.ts`、`use-table-url-state.ts`、`use-status.ts` |
| `src/features/<feature>/hooks/` | 单一功能模块内的业务 hooks | `features/wallet/hooks/use-payment.ts`、`features/system-settings/hooks/use-update-option.ts`、`features/rankings/hooks/use-rankings.ts` |
| `src/components/<组件>/hooks/` | 只服务某个复杂通用组件 | `components/data-table/hooks/use-data-table.ts` |

历史遗留:`src/lib/` 下有两个底层 hooks(`use-chart-theme.ts`、`use-controllable-state.ts`),不要向 `lib/` 新增 hooks。

导入方式:主流是直接按文件路径导入(`import { useStatus } from '@/hooks/use-status'`)。`src/hooks/index.ts` 与部分 feature 的 `hooks/index.ts`(wallet/home/pricing/playground/profile)是 barrel 再导出,存在但非强制;新 hook 不要求登记 barrel,但所在目录已有 barrel 时应同步补一行。

---

## Naming & File Conventions

- 文件名:kebab-case,以 `use-` 开头,后缀 `.ts`(`use-copy-to-clipboard.ts`)。
- 导出:命名导出 `export function useXxx(...)`,hook 名 camelCase 且以 `use` 开头。禁止默认导出——`src/hooks/use-dialog.ts` 末尾的 `export default useDialogState` 已标注 `@deprecated`,不要模仿。
- 一个文件可含多个强相关 hooks(`use-dialog.ts` 同时导出 `useDialog` / `useDialogState` / `useDialogs`),但主题必须单一。
- 类型:参数用 `type XxxOptions` / `XxxParams`,返回值多字段时定义 `XxxReturn` 或返回明确 shape 的对象;避免 `any`(`web/default/AGENTS.md` 3.2)。
- 文件头必须保留/携带 QuantumNous AGPL 版权注释(现有 65 个 hooks 文件全部有,受根 `AGENTS.md` 保护条款约束,不得删除)。

---

## When to Extract a Hook

抽取时机(依据 `web/default/AGENTS.md` 3.3 与根 `AGENTS.md` 代码质量条目):

- 组件单文件超过约 200 行,且其中有可独立的 stateful 逻辑 → 抽到同 feature 的 `hooks/`。
- 同一段逻辑被 ≥2 个组件使用 → 按复用范围放入 feature `hooks/` 或 `src/hooks/`。
- React Query 的 `useQuery`/`useMutation` 若含 queryKey 约定、invalidate 联动、toast 等副作用,应封装成 feature hook(如 `use-update-option.ts`);纯一次性、无联动的查询允许直接写在组件里(现状如此,不强制包装)。
- 反之:只有一个调用方、不表达稳定业务概念、纯粹为缩短组件的机械抽取——不要抽,直接内联(根 `AGENTS.md` "Common Code Quality")。

Hook 与 Zustand store 的分界(`web/default/AGENTS.md` 3.5):跨页面全局状态放 `src/stores/`(`auth-store.ts`、`system-config-store.ts`、`notification-store.ts`);服务端数据用 React Query hook;组件局部可复用逻辑才是自定义 hook。hook 内需要读写 store 时,渲染路径用选择器订阅,非渲染路径(如 `queryFn` 内)用 `useXxxStore.getState()`——见 `src/hooks/use-status.ts`。

---

## Data Fetching Hooks (React Query)

查询 hook 的标准形态——薄封装 `useQuery`,API 调用来自 feature 的 `api.ts` 或 `src/lib/api`:

```ts
// web/default/src/features/rankings/hooks/use-rankings.ts
export function useRankings(period: RankingPeriod) {
  return useQuery({
    queryKey: ['rankings', period],
    queryFn: () => getRankings(period),
    staleTime: 5 * 60 * 1000,
  })
}
```

- `queryKey` 用数组形式,首元素为资源名,参数依次追加(`['status']`、`['rankings', period]`、`['system-options']`)。同一资源的 key 在整个 feature 内保持一致,否则 invalidate 会失效。
- `staleTime`/`gcTime` 按数据特性在 hook 内配置,不放调用方。
- 需要 localStorage 兜底的查询用 `placeholderData` + 初始化函数(见 `src/hooks/use-status.ts` 的 `getInitialStatus`)。

变更 hook 用 `useMutation`,成功后 invalidate 关联 query 并 toast:

```ts
// web/default/src/features/system-settings/hooks/use-update-option.ts(节选)
export function useUpdateOption() {
  const queryClient = useQueryClient()
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
    onError: (error: Error) => {
      toast.error(error.message || i18next.t('Failed to update setting'))
    },
  })
}
```

注意 dashboard API 的契约:HTTP 恒 200,成败看 body 的 `success` 字段,所以 `onSuccess` 里仍要判 `data.success` 并走失败 toast 分支。

简单的命令式动作(无缓存联动)也可以用 `useState` + `useCallback` 手写 loading,如 `features/wallet/hooks/use-redemption.ts` 的 `redeemCode`;两种写法并存,新代码优先 `useMutation`。

---

## UI / Utility Hooks

- 返回值形态:状态类 hook 返回 tuple 并 `as const`(`useDialog(): readonly [boolean, DialogHandlers]`);多字段结果返回对象(`useRedemption()` 返回 `{ redeeming, redeemCode }`)。
- 返回的对象/回调必须 `useMemo`/`useCallback` 稳定化,避免下游依赖数组失效——参照 `src/hooks/use-dialog.ts` 中 handlers 的写法。
- 副作用必须清理:定时器、事件监听在 effect 返回函数或卸载 effect 中清除(`use-debounce.ts` 的 `clearTimeout`、`use-mobile.ts` 的 `mql.removeEventListener`、`use-copy-to-clipboard.ts` 卸载时清 timeout)。
- localStorage 读写一律 try/catch 包裹并给出降级值(`use-status.ts`、`use-table-url-state.ts` 的 `getStoredPageSize`),Safari 隐私模式等环境会抛异常。
- 与 TanStack Router 解耦:通用 hook 不直接 import 路由,由调用方注入 `search`/`navigate`(见 `src/hooks/use-table-url-state.ts` 的 `NavigateFn` 参数),保证 hook 可测试、可跨路由复用。

### i18n in hooks

- hook 返回的、参与渲染的文案:必须经 `useTranslation()` 的 `t()`,保证语言切换时重渲染(`use-copy-to-clipboard.ts`)。
- 事件回调中一次性求值的 toast 文案:`useTranslation()` 或 `import i18next` 后 `i18next.t(...)` 均为现状可接受写法(`use-redemption.ts`、`use-update-option.ts`),因其在调用瞬间求值。
- key 一律用英文原文(flat JSON 约定,根 `AGENTS.md`);禁止 toast 硬编码非 i18n 文案。

---

## Forbidden Patterns

- **禁止**违反 Rules of Hooks:条件/循环/嵌套函数内调用 hook;自定义 hook 只能在函数组件或其他 hook 中调用。
- **禁止** hook 文件使用默认导出;统一命名导出(`use-dialog.ts` 的 default export 是待清理遗留)。
- **禁止**同名 `.ts`/`.tsx` 并存(`use-mobile.ts` 与 `use-mobile.tsx` 是历史重复,不要新增此类情况,也不要再引入第三份实现)。
- **禁止**在 hook 中直接创建 axios 实例或裸 `fetch`;HTTP 调用只能来自 feature 的 `api.ts` 或 `src/lib/api`(统一实例含 baseURL/拦截器,`web/default/AGENTS.md` 3.6)。现有 hooks 仅允许 `import type { AxiosRequestConfig }` 这类纯类型引用。
- **禁止**裸访问 localStorage(无 try/catch)。
- **禁止** mutation 成功后遗漏 `invalidateQueries`——写变更 hook 时必须梳理受影响的 queryKey(参照 `use-update-option.ts` 的 `STATUS_RELATED_KEYS` 联动)。
- **禁止**为绕过 dashboard `success:false` 判断而只写 `onError`;HTTP 200 + `success:false` 是常态失败路径。
- **禁止**返回未稳定化的对象/函数字面量导致调用方 effect 每次重跑。
- **禁止**删除或改写文件头的 QuantumNous 版权注释(根 `AGENTS.md` Protected project information)。
- **禁止**把 default 主题的 hooks 直接复制进 classic(依赖栈不同:TanStack Query/Zustand vs Semi Design/JS),classic 侧改动遵循其 `src/hooks/<domain>/useXxx.jsx` 既有模式(如 `web/classic/src/hooks/common/useNotifications.js`)。

---

## References

- `web/default/src/hooks/` — 通用 hooks 全集;`use-dialog.ts`(返回形态范例)、`use-status.ts`(Query + store 同步 + localStorage 兜底)、`use-table-url-state.ts`(路由解耦)
- `web/default/src/features/*/hooks/` — feature 业务 hooks;`system-settings/hooks/use-update-option.ts`(mutation + invalidate 范例)
- `web/default/src/components/data-table/hooks/` — 组件级 hooks
- `web/default/AGENTS.md` — 3.1 i18n、3.3 组件(200 行拆分阈值)、3.5 状态管理、3.6 API 请求
- 根 `AGENTS.md` — 前端条目(bun、i18n flat JSON)、Common Code Quality(单调用方助手不抽取)、Protected project information;其 JSON 包装(`common.Marshal`)、DB 三库兼容、`lockForUpdate(tx)`、`common/quota_math.go` 等条款为后端约束,不适用于前端 hooks,但同仓协作时不得与之冲突
