# Component Guidelines

> 前端组件规范。内部项目精简版。

---

## Overview

- 本规范约束 `web/default/`(主主题,React 19 + TypeScript + Rsbuild + Base UI + Tailwind CSS v4)。`web/classic/` 为旧主题(React 18 + Semi Design),仅沿用其目录内既有写法,不适用本文的 ui/Tailwind 体系。
- 一律使用 React 函数组件 + Hooks,单一职责;单文件超过约 200 行时拆分子组件或抽自定义 Hook(见 hook-guidelines.md)。
- 上位规范:根 `AGENTS.md` 与 `web/default/AGENTS.md`,本文与其冲突时以它们为准。
- 改动 `.ts`/`.tsx` 后必须在 `web/default/` 下执行 `bun run typecheck`(tsgo)与 `bun run lint`(oxlint),error 清零。

---

## Directory Layout

| 位置 | 内容 |
| --- | --- |
| `web/default/src/components/ui/` | shadcn 风格 registry 基础组件(Base UI + cva),由 `components.json` 管理,只封装样式与交互,无业务逻辑 |
| `web/default/src/components/` | 项目级公共组件:`confirm-dialog.tsx`、`status-badge.tsx`、`multi-select.tsx`、`data-table/`、`layout/` 等 |
| `web/default/src/features/<feature>/components/` | 功能组件(表格列、行操作、弹窗、抽屉),只服务本 feature |
| `web/default/src/lib/` | 通用工具(`utils.ts` 的 `cn()`、`api.ts` 等) |
| `web/default/src/styles/` | 全部自定义 CSS:`index.css`、`theme.css`、`theme-presets.css` |

归属判定遵循 `web/default/src/components/data-table/README.md`:

> Keep feature-specific columns, actions, and dialogs inside their feature folders. Shared table code belongs here only when it is reusable across more than one feature.

即:先写在 feature 内,被 2 个以上 feature 复用后才上移到 `src/components/`。`data-table/` 等带 `index.ts` 的公共包只从其公共入口导入(`@/components/data-table`),不要深入内部路径。

---

## Naming Conventions

- 文件名 kebab-case,组件名 PascalCase,**具名导出**(不用 default export):`api-keys-mutate-drawer.tsx` 导出 `ApiKeysMutateDrawer`。
- feature 组件以 feature 名作前缀:`ApiKeysTable`、`ApiKeysPrimaryButtons`、`ApiKeysDeleteDialog`(`web/default/src/features/keys/components/`)。
- Props 类型命名 `<Component>Props`,用 `type` 别名或 `interface`(需继承 DOM 属性时用 `interface X extends React.HTMLAttributes<...>`)。
- 常用后缀约定:`*-dialog`(弹窗)、`*-mutate-drawer`(创建/编辑抽屉)、`*-columns`(表格列定义)、`*-provider`(feature 级 context)、`*-primary-buttons`(页头操作按钮)。
- 类型仅作类型导入时用 `import type { X } from '...'`;禁止 `any`,优先具体类型或 `unknown`。

---

## Props Conventions

- 每个组件的 props 必须有显式类型。公共组件示例(`web/default/src/components/confirm-dialog.tsx`):

```tsx
type ConfirmDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: React.ReactNode
  desc: React.JSX.Element | string
  destructive?: boolean
  handleConfirm: () => void
  isLoading?: boolean
  className?: string
  children?: React.ReactNode
}

export function ConfirmDialog(props: ConfirmDialogProps) { ... }
```

- feature 弹窗/抽屉统一受控三件套:`open` + `onOpenChange` + 可选 `currentRow`(有 `currentRow` 即编辑态,`const isUpdate = !!currentRow`),见 `web/default/src/features/keys/components/api-keys-mutate-drawer.tsx`:

```tsx
type ApiKeyMutateDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  currentRow?: ApiKey
}
```

- 可复用展示组件必须透传 `className` 并用 `cn()` 合并;包装 DOM 元素时 `...props` 透传剩余原生属性,ui 基础组件加 `data-slot` 标识(见 `web/default/src/components/ui/button.tsx` 的 `data-slot='button'`)。
- 遵循 `web/default/AGENTS.md`:props 非必要不解构,整体接收 `props` 后按需访问/局部解构(如 `ConfirmDialog`、`StatusBadgeList`)。
- 可选布尔 props 给默认值(`copyable = true`、`disabled = false`),不要靠调用方猜默认行为。

---

## Events & Callbacks

- 回调 props 命名 `onXxx`,组件内部处理函数命名 `handleXxx`;受控开关一律 `onOpenChange(open: boolean)`,与 Base UI/registry 组件保持同名。
- 包装原生事件时先做组件行为,再链式调用外部回调(`web/default/src/components/status-badge.tsx`):

```tsx
const handleClick = (e: React.MouseEvent<HTMLSpanElement>) => {
  if (copyable) {
    e.stopPropagation()
    copyToClipboard(copyText || label || '')
  }
  onClick?.(e)
}
```

- feature 内多个弹窗/抽屉的开关状态不逐层传 props,统一放 feature provider,消费方通过 hook 触发(`web/default/src/features/keys/components/api-keys-primary-buttons.tsx`):

```tsx
const { setOpen } = useApiKeys()
<Button size='sm' onClick={() => setOpen('create')}>
```

- 数据变更走 React Query `useMutation`,成功后 `invalidateQueries` + `toast`;服务端错误统一 `handleServerError`(`web/default/src/lib/handle-server-error.ts`)。

---

## Styling (Tailwind / Theme)

- 只用 Tailwind 工具类;动态/条件类名必须经 `cn()`(clsx + tailwind-merge,`web/default/src/lib/utils.ts`)合并,禁止手写字符串拼接。
- 多变体组件用 `cva` 定义 `variant`/`size`,配 `VariantProps` 推导类型(`web/default/src/components/ui/button.tsx`):

```tsx
const buttonVariants = cva('inline-flex items-center ...', {
  variants: {
    variant: { default: 'bg-primary text-primary-foreground ...', outline: '...', destructive: '...' },
    size: { default: 'h-8 ...', sm: '...', icon: 'size-8' },
  },
  defaultVariants: { variant: 'default', size: 'default' },
})
```

- 颜色只用语义 token(`bg-primary`、`text-muted-foreground`、`bg-destructive/10` 等),token 由 `web/default/src/styles/theme.css` 的 `@theme inline` + CSS 变量定义(多主题预设在 `theme-presets.css`)。不要写死 `bg-blue-500` 之类的调色板色。
- 暗色模式:`.dark` class 驱动(`theme.css` 中 `@custom-variant dark (&:is(.dark *))`),组件里直接用 `dark:` 前缀,不写 `prefers-color-scheme` 媒体查询。
- 响应式移动优先,用 `sm:`/`md:`/`lg:` 断点;必须写自定义 CSS 时集中到 `src/styles/`,组件文件内不建独立 CSS。
- 图标以 `lucide-react` 为主(现状约 265 个文件使用),尺寸用 Tailwind(`size-4` 等);装饰性图标加 `aria-hidden='true'`。

---

## i18n in Components

- 用户可见文案必须 `useTranslation()` + `t('English key')`,key 即英文原文,locale 文件为 `web/default/src/i18n/locales/{lang}.json` 的 flat JSON。真实示例:`t('Create API Key')`(`api-keys-primary-buttons.tsx`)。
- 子组件自行调用 `useTranslation()`,不依赖父组件传入 `t`。
- feature `constants.ts` 中的 `SUCCESS_MESSAGES`/`ERROR_MESSAGES` 存的是 i18n key,展示时必须包 `t()`:`toast.success(t(SUCCESS_MESSAGES.API_KEY_CREATED))`。

---

## Forbidden Patterns

- **手改 `web/default/src/routeTree.gen.ts`** —— 由 `@tanstack/router-plugin` 构建时自动生成(`rsbuild.config.ts`)。
- **删除/改写文件头 AGPL 版权注释,或移除 new-api / QuantumNous 品牌信息** —— 根 `AGENTS.md` 项目治理条款,格式化脚本 `scripts/format-with-protected-headers.mjs` 亦保护头部;新建源文件需带同款头。
- **硬编码用户可见文案**(绕过 `t()`,含直接渲染消息常量)。
- **`any` 类型;2 层及以上嵌套三元表达式**(改 if-else / 提前返回)。
- **非动态场景写内联 `style`;在 `components/ui/` 之外新建组件级 CSS 文件**(自定义样式集中 `src/styles/`)。
- **在 `components/ui/` 中写业务逻辑或发 API 请求** —— registry 基础组件只做样式与交互封装。
- **feature 专属的列定义、行操作、弹窗放进 `src/components/`**(见 data-table README 归属规则)。
- **直接操作 `window.location` 导航** —— 用 TanStack Router 的 `useNavigate` / `Link`。
- **手写字符串拼接 className 代替 `cn()`**;**绕过 `@/components/data-table` 等公共包的 `index.ts` 深层导入**。
- **在 `web/classic/` 中引入 `web/default/` 的 ui/Tailwind 组件体系**(两主题技术栈隔离)。

---

## Verification

```bash
cd web/default
bun run typecheck   # tsgo -b,零 error
bun run lint        # oxlint -c .oxlintrc.json .,零 error
bun run format      # oxfmt(保护版权头)
```
