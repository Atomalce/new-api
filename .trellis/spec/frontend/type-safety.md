# Type Safety Guidelines

> 前端 TypeScript 类型安全规范。内部项目精简版。

---

## Overview

本规范只约束主主题 `web/default/`(React 19 + TypeScript + Rsbuild + TanStack Router)。旧主题 `web/classic/` 为纯 JavaScript(`jsconfig.json`,prettier/eslint),不参与 TypeScript 类型检查,本文不适用。

核心原则:

- `strict: true` 全量开启,typecheck 零错误是交付前提(`web/default/AGENTS.md` §3.2 的硬性要求)。
- HTTP 响应、localStorage、URL search params 是类型信任边界:要么显式标注返回类型,要么用 Zod 做运行时校验。
- `any` 只减不增,默认替代是 `unknown` + 类型收窄。

---

## tsconfig Strictness

`web/default/tsconfig.json` 是 project references 入口,实际配置在两个子文件:

- `web/default/tsconfig.app.json` — `src/` 应用代码
- `web/default/tsconfig.node.json` — `rsbuild.config.ts`

已开启的严格选项(摘自 `web/default/tsconfig.app.json`):

```jsonc
"strict": true,
"noUnusedLocals": true,
"noUnusedParameters": true,
"noFallthroughCasesInSwitch": true,
"noUncheckedSideEffectImports": true,
"isolatedModules": true,
"moduleResolution": "Bundler",
"noEmit": true
```

约束:

- 禁止调低任何已开启的严格选项。
- `noUncheckedIndexedAccess` / `exactOptionalPropertyTypes` 当前未开启;不要按已开启的语义写代码,也不要私自开启(会引发全库报错,需单独立项处理)。
- 路径别名 `@/*` → `./src/*`,在 `tsconfig.json`/`tsconfig.app.json` 与 `rsbuild.config.ts` 的 `resolve.alias` 中各配置一份,修改时必须同步。
- `noEmit: true`:类型检查与打包完全分离,产物由 Rsbuild 生成,typecheck 只做校验。

---

## Type Check Commands

在 `web/default/` 目录下执行,包管理器统一用 bun:

| 命令 | 实际执行 | 用途 |
| --- | --- | --- |
| `bun run typecheck` | `tsgo -b` | 类型检查(tsgo 为 `@typescript/native-preview`,非 tsc) |
| `bun run build:check` | `tsgo -b && rsbuild build` | CI / 交付前完整检查 |
| `bun run lint` | `oxlint -c .oxlintrc.json .` | lint(含 typescript 插件规则) |

规则(引自 `web/default/AGENTS.md` §3.2):每次改动 `.ts`/`.tsx` 后必须执行 typecheck 并修复至零错误,不得遗留;lint error 必须清零,warning 按变更范围与风险评估。

---

## API Response Types

后端统一响应契约为 `{ success, message?, data? }`。响应类型按 feature 就近定义在 `web/default/src/features/<feature>/types.ts`,`ApiResponse<T>` 当前在各 feature 内独立声明(现状如此,新 feature 沿用此模式,不要引入新的共享位置):

```ts
// web/default/src/features/keys/types.ts
export interface ApiResponse<T = unknown> {
  success: boolean
  message?: string
  data?: T
}
```

**API 函数必须显式标注返回类型。** 统一 axios 实例(`web/default/src/lib/api.ts`)调用时不传泛型,`res.data` 是 `any`,函数签名是唯一的类型边界:

```ts
// web/default/src/features/keys/api.ts
export async function getApiKey(id: number): Promise<ApiResponse<ApiKey>> {
  const res = await api.get(`/api/token/${id}`)
  return res.data
}
```

仅单处使用的响应类型可就近定义在使用文件内(如 `StatusApiResponse`,`web/default/src/hooks/use-system-config.ts`);跨组件复用的放 feature 的 `types.ts`。

---

## Zod at Trust Boundaries

实体与表单类型优先从 Zod schema 推导,不手写平行的 interface:

```ts
// web/default/src/features/keys/types.ts
export const apiKeySchema = z.object({
  id: z.number(),
  name: z.string(),
  status: z.number(), // 1: enabled, 2: disabled, 3: expired, 4: exhausted
  // ...
})
export type ApiKey = z.infer<typeof apiKeySchema>
```

必须做运行时校验(而非仅类型断言)的场景:

- **URL search params**:路由文件用 `validateSearch` + zod schema,字段带 `.catch()` 兜底非法值(`web/default/src/routes/_authenticated/keys/index.tsx` 的 `apiKeySearchSchema`)。
- **localStorage**:读出后必须 `schema.parse` 再使用(`web/default/src/features/playground/lib/storage/storage.ts`)。
- **表单**:React Hook Form + `@hookform/resolvers/zod`,表单值类型用 `z.infer<typeof schema>`(`web/default/AGENTS.md` §3.7)。

---

## any / Assertion Policy

oxlint 相关规则(`web/default/.oxlintrc.json`):

| 规则 | 级别 |
| --- | --- |
| `typescript/no-explicit-any` | warn |
| `typescript/no-non-null-assertion` | error |
| `typescript/no-unnecessary-type-assertion` | error |
| `typescript/consistent-type-imports` | error(inline 风格:`import { type X }`) |

态度与替代:

- 新代码不写 `any` / `as any`。需要"类型未知"时用 `unknown` 并收窄,参考 `handleServerError(error: unknown)`(`web/default/src/lib/handle-server-error.ts`)与 `toNumber(value: unknown, fallback: number)`(`web/default/src/hooks/use-system-config.ts`)。
- 存量 `as any` 约 10 处(集中在 system-settings 动态表单字段名等处),属技术债,只减不增。
- 校验字面量结构优先用 `satisfies` 而非 `as`(如 `} satisfies ApiRequestConfig`,`web/default/src/features/channels/hooks/use-channel-upstream-updates.ts`)。
- 双重断言 `as unknown as T` 仅允许用于第三方库类型缺口(典型:`zodResolver(schema) as unknown as Resolver<Values>`,`web/default/src/features/system-settings/general/checkin-settings-section.tsx`);禁止用它绕过业务数据的校验。
- 非空断言 `!` 为 lint error,新代码用提前返回、`?.`、显式判空替代。

---

## Ambient Declarations & Module Augmentation

- 全局声明与无类型库的 shim 放 `src/` 根下 `.d.ts`,勿散落各 feature:
  - `web/default/src/env.d.ts` — rsbuild 类型引用 + `@visactor/react-vchart` 等模块 shim,shim 的宽泛值类型用 `Record<string, unknown>`,不用 `any`。
  - `web/default/src/tanstack-table.d.ts` — `declare module '@tanstack/react-table'` 扩展 `ColumnMeta`(mobile 布局字段)。
- 需要给第三方库的配置对象加自定义字段时,用 module augmentation 而不是断言,如 `web/default/src/lib/api.ts` 中扩展 `AxiosRequestConfig` 增加 `skipBusinessError` / `skipErrorHandler` / `disableDuplicate`。

---

## Generated & Excluded Files

- `web/default/src/routeTree.gen.ts`:由 `@tanstack/router-plugin` 自动生成(文件头带 `@ts-nocheck`),禁止手改;oxlint `ignorePatterns` 已排除。
- `web/default/src/components/ui/`:shadcn 生成的基础组件,oxlint 排除,但仍在 typecheck 范围内,改动后同样要过 `bun run typecheck`。

---

## Forbidden Patterns

- 调低 `tsconfig.app.json` / `tsconfig.node.json` 中任何已开启的严格选项。
- 在业务代码中使用 `@ts-ignore` / `@ts-expect-error` / `@ts-nocheck`(现状:`src/` 内仅生成文件 `routeTree.gen.ts` 含 `@ts-nocheck`,保持为零)。
- 新增显式 `any`、`as any`;新增非空断言 `!`。
- API 函数省略返回类型标注直接 `return res.data`(`res.data` 是 `any`,类型安全会静默丢失)。
- 与 Zod schema 平行手写重复的实体/表单类型 —— 用 `z.infer`。
- 未经 schema 校验直接消费 localStorage 或 URL search params。
- 值风格导入仅作类型使用 —— 必须 `import type` 或 inline `type` 修饰(`typescript/consistent-type-imports` 为 error)。
- 手改 `routeTree.gen.ts` 或其它生成文件。
- 在 `web/classic/`(JavaScript)中新增 TypeScript 文件或对其套用本规范 —— 该主题不在类型检查体系内。
