# 合并归档：请求指纹方案的代码级调研成果

> 来源：`07-11-upstream-usage-cache-expiry-billing`（2026-07-10 18:18 完成的较早规划迭代，
> 已于 2026-07-10 合并入本任务并删除原目录）。
> 本任务的 prd.md / design.md / research.md（2026-07-11 重新对齐）是较新的权威版本，
> 已吸收该迭代的代码调研发现并修订了部分决策。**决策冲突处一律以本任务 prd.md / design.md 为准**；
> 本文件保留的价值是与决策无关的代码级锚点与实现陷阱（file:line 基于 2026-07-10 工作区，实现时以符号定位为准）。
>
> 2026-07-11 再次对齐：任务范围收窄为仅明确识别的 Codex `/v1/responses`
> 流量、固定 180 秒周期；用户已选择 `prompt_cache_key` / `Session_id`
> cache lineage 作为“同一个请求”，完整请求体指纹方案已拒绝。
> 本文件中关于 chat/completions、Claude 原生、音频流式等路径的发现降级为参考资料；
> 对 Responses 路径仍直接有效的是发现 5、6、9、10、12、13（Responses 部分）。

## 决策冲突对照（旧迭代 → 本任务生效决策)

| 维度 | 旧迭代（本文件附录） | 本任务（生效） |
| --- | --- | --- |
| 缓存身份 | `sha256(user_id + origin_model + 原始 body 哈希)` 请求指纹 | `prompt_cache_key` / Codex `Session_id` lineage + HMAC-SHA256；完整 body 指纹及静默回退均拒绝 |
| Redis 故障 | fail-closed：判定失败视为过期，按全价（只多收不少收） | fail-open：保留上游折扣，绝不因 Redis 故障多收费（见 research.md "Rejected behavior alternatives"） |
| 配置面 | 运行时 option `cache_expiry_setting.interval_seconds`（默认 300，0=禁用，热更新） | 始终启用、周期固定 180 秒、无 feature flag/OptionMap/UI；Q3 待定无 Redis 启动行为 |
| 无 Redis 部署 | 退化为单实例内存窗口（sync.Mutex + map + janitor） | 启用即强制要求健康共享 Redis + 显式共享密钥，禁止进程本地状态 |
| OpenRouter 渠道 | 整体排除（计费分支 PromptTokens 语义倒挂 + Cost 反推依赖原始 cached_tokens） | 覆盖：`CalcOpenRouterCacheCreateTokens` 改读调整前快照，公开字段用调整后 usage |
| passthrough-body | 整体跳过（不改计费也不改响应） | 计费策略覆盖字节透传：客户端可见 usage 必须与账单一致 |
| 音频模型流式 | 整体跳过（usage 位于已发出的倒数第二 chunk；逐 chunk 探测被认为成本不值） | design 要求 patch 非最终音频 usage chunk —— 意味着必须逐 chunk 在发出前检查，实现时需接受该成本或显式重新决策 |
| 窗口 TTL | 固定 300 秒 | 固定 180 秒，不刷新 |
| 双侧不一致的中间态 | 接受流式中间 chunk 残留未清零字段（已知限制） | 拒绝：凡发出 usage 必须与结算快照一致 |

## 对本任务仍然直接有效的关键发现（与上述决策无关）

1. **流式 struct mutation 必须钉死在流末执行一次**：Claude `FormatClaudeResponseInfo` 的
   message_delta 分支（relay-claude.go:751-767）会用上游值无条件覆盖
   `claudeInfo.Usage.PromptTokens/CachedTokens`——在 message_start 处改 struct 会被撤销；
   Gemini 流式 :1374 每 chunk 覆盖 `*usage` 同理。事件级只做字节 patch。
2. **Claude 清零 patch 必须排在既有 fill-only patch 之后**：
   `patchClaudeMessageDeltaUsageData`（relay-claude.go:815-817）中 `setMessageDeltaUsageInt`
   （:699-712）对"存在但为 0"的字段会用未 mutation 的 claudeInfo.Usage 重新注入原值。
3. **OpenAI 流式判定前必须先归一化信号**：`applyUsagePostProcessing`
   （relay/channel/openai/usage.go:10-52）在最终 chunk 发出（relay-openai.go:173）之后（:182）
   才执行——DeepSeek/Zhipu/Moonshot/llama.cpp 的缓存信号在发出时刻还不在标准字段，
   需在 :173 前对 `lastStreamData` 做等价提取。
4. **音频模型流式的 usage 位置**：`isAudioModel` 路径 usage 位于倒数第二个 chunk
   （relay-openai.go:127-131 循环内已发出，:146-162 事后提取）——patch lastStreamData 无效。
5. **跨渠道重试的门控/结果分层**：per-attempt 属性（渠道类型、passthrough 开关）必须每次实时求值，
   只有窗口 claim 结果可以 memoize 跨 attempt 复用；合并 memoize 会让重试绕过门控。
   重试循环位于 controller/relay.go:191-237，attempt 间 RelayInfo 字段会遗留。
6. **亲和观测需要 5 字段快照**：`observeChannelAffinityUsageCache`
   （service/channel_affinity.go:864-875, 913-929）除三个缓存信号外还累加
   PromptTokens/TotalTokens（:873-874），Claude 命中率分母含 prompt（:891-899）——
   只还原三信号会系统性低估命中率。三个信号字段定义见 dto/openai_response.go:227, 235, 256
   （`InputTokensDetails` 是指针，需 nil 守卫）。
7. **计费语义判定的既有工具**（service/text_quota.go，同包可复用）：
   `usageSemanticFromUsage`（:326-334，只看 UsageSemantic 与最终 relay format，不看 token 数）、
   `isLegacyClaudeDerivedOpenAIUsage`（:71-82，只看 creation 5m/1h 拆分字段，依赖
   UsageSemantic/UsageSource 为空——策略不得写这两个字段）、Claude 语义加回门在 :256。
8. **OpenRouter 计费分支的具体倒挂位置**：text_quota.go:214-228（:219 先减 CR）+
   service/quota.go:261-280（Cost 反推缓存写入量依赖原始 cached_tokens）——
   这正是本任务要求 `CalcOpenRouterCacheCreateTokens` 读调整前快照的原因。
9. **上游 usage 数字不受请求侧约束**：`maxTokensLimit` 只约束客户端请求字段
   （relay/helper/valid_request.go:117-120），上游返回的 PromptTokens/CachedTokens
   可声称任意大值；加法必须饱和（钳制 int32 + 审计），溢出为负会经 text_quota.go:301
   的 quota≤0→1 分支变成上游可触发的少收费。
10. **仓库原语现状**：全仓无 Redis SetNX wrapper（需在 common/redis.go 新增）；
    请求 body 已由 Distribute 中间件缓存（middleware/distributor.go:209，
    `common.GetBodyStorage(c)`，生命周期覆盖计费时刻 body_cleanup.go:11-22，
    大 body 可能落磁盘 body_storage.go:22）；内存窗口先例 common/rate-limit.go:8-42；
    passthrough 检查依赖 `model_setting.GetGlobalSettings().PassThroughRequestEnabled` 与
    `info.ChannelSetting.PassThroughBodyEnabled`（service 直接导入 relay/channel/claude
    会成环——relay-claude.go:18 已导入 service）。
11. **格式转换 handler 家族**（不 hook 则功能静默失效）：
    `chat_via_responses.go`（OaiResponsesToChatHandler :23 / Buffered :75 / Stream :178，
    由 compatible_handler.go:74-92 触发，usage 经 relayconvert/responses_to_chat.go:152 映射）、
    `gemini/relay_responses.go`（:21 / :73，gemini/adaptor.go:262-264 分派）、
    `responses_via_chat.go`（advancedcustom/adaptor.go:234-236 触发）。
    渠道收敛：AWS Bedrock（relay-aws.go:248/279）、Vertex（adaptor.go:331-360）、
    Azure（OpenAI adaptor）全部收敛进格式 handler，hook 一次全渠道生效。
12. **Responses 流式终态事件不只 response.completed**：response.done / response.incomplete
    也可携带 usage（chat_via_responses.go:109 佐证）——patch 条件应为"事件含 usage 缓存信号"
    （本任务的 claim 规则另行要求 incomplete/failed 不得 claim，两者不矛盾：
    patch 覆盖面 ≠ claim 资格）。
13. **每格式拦截点速查**（透传 vs re-marshal 决定字节 patch 是否必要）：
    OpenAI chat 非流式透传分支 :271→:289、re-marshal 分支 :249/:266/:273-287；
    Responses 非流式 :44 写出早于 :47 usage 构建（需重排为 parse→adjust→patch→send）、
    流式 :86 unmarshal 先于 :91 发出；Claude 非流式 usage 拷贝 :909-918、
    RelayFormatClaude 透传 :936、RelayFormatOpenAI :923（buildOpenAIStyleUsageFromClaudeUsage
    :609-627）；流式 struct mutation 唯一安全点：HandleStreamFinalResponse 内、
    UsageSemantic 设置（:855）之后、:862/:893 之前；Gemini 非流式 :43 构建 usage、:45 写出，
    流式回调 :85 StringData 前 patch、geminiStreamHandler 返回点 relay-gemini.go:1397。

---

# 附录 A：原 prd.md（逐字保留）

# 上游 Usage 缓存周期性失效与全价计费

## Goal

在网关层实现缓存过期机制：每隔 N 秒（可配置，默认 300），对**同一请求内容**（请求指纹）强制执行一次"缓存失效"——将上游返回的 cached_tokens 归零，全部输入按全价计费，**同时客户端收到的响应 usage 也同步修改**；窗口内的后续相同请求恢复正常缓存折扣。

## 术语

- **请求指纹（fingerprint）**：`sha256(user_id + origin_model + 请求体哈希)`。同一用户、同一模型、字节级相同的请求体视为"同一个请求"。（用户决策 2026-07-10：维度为请求指纹，不是 API key/token_id。）
- **缓存新鲜窗口**：某指纹最近一次全价计费后的 N 秒。窗口内该指纹的请求正常享受缓存折扣。

## Requirements

1. **按请求指纹追踪**：以请求指纹为维度记录缓存新鲜窗口，跨渠道重试保持稳定（指纹基于原始客户端请求体，不受渠道映射影响）。
2. **过期行为 — 清零一次**：
   - 仅对上游返回 `cached_tokens > 0` 的请求触发判断；无缓存命中的请求完全不受影响。
   - 指纹对应的新鲜窗口不存在（首次出现或已过期）→ 本次请求：cached_tokens 归零、全部输入按全价计费，并开启新的 N 秒窗口。
   - 窗口存在 → 正常缓存折扣，不做任何修改。
   - 决策：指纹**首次**出现且带缓存命中时即按全价计一次（SETNX 语义天然如此，且不会少收费）。
3. **全局生效**：所有渠道、所有模型统一适用，无按渠道/模型开关。覆盖 OpenAI Chat Completions、OpenAI Responses、Claude Messages、Gemini 原生四种格式（流式+非流式）；AWS/Vertex/Azure 等渠道收敛进这四个格式处理器，自动覆盖。两个例外（代码调研得出，理由见 design §5）：
   - **OpenRouter 渠道整体排除**：其计费分支的 PromptTokens 语义倒挂 + Cost 反推缓存写入量依赖原始 cached_tokens，任一 mutation 方向都会算错账（双计或破坏 cache_creation 分离）。
   - **passthrough-body 模式的请求整体跳过**（不改计费也不改响应）：该模式要求网关不触碰响应字节，与"必须改响应"冲突；跳过以保持双向一致不变量。
   - **音频模型（gpt-4o-audio 系）流式整体跳过**：该路径的 usage 位于流中已发出的倒数第二个 chunk，响应字节不可改；为保双向一致，计费侧同步跳过（对抗审查确认，见 design §5 例外 3）。
4. **双向一致修改**（用户决策 2026-07-10：客户端响应必须改）：
   - 计费侧：缓存部分按全价 prompt 输入计费，无缓存折扣；计费日志中 cache_tokens=0。
   - 客户端响应侧：返回的 usage 中缓存读取字段清零，输入 token 字段体现全量输入。流式与非流式都必须覆盖。
   - 两侧必须基于**同一次**过期判定（同一次原子判断的结果），不允许计费判过期而响应没改、或反之。
   - 按格式的字段语义：
     - OpenAI Chat Completions / Responses：`prompt_tokens`（`input_tokens`）本身已包含缓存量 → 仅将 `prompt_tokens_details.cached_tokens`（`input_tokens_details.cached_tokens`）清零，prompt_tokens 不变。
     - Claude Messages：`input_tokens` 不含缓存量 → `input_tokens += cache_read_input_tokens`，然后 `cache_read_input_tokens = 0`。
5. **可配置间隔**：过期间隔为系统设置项，单位秒，默认 300，0 = 禁用整个功能。无需管理端 UI（可通过通用 option API 配置），前端 UI 可后置。
6. **cache_creation 不受影响**：缓存写入 token（`cache_creation_input_tokens` / 5m / 1h 细分）不参与此过期机制，计费与响应均保持原样。
7. **计费安全不变量**（遵守 AGENTS.md billing safety invariants）：
   - 任何 token 加法都不得溢出产生负值；修改后的 usage 走既有的 `common/quota_math.go` 转换路径。
   - 过期强制全价只会**多收不少收**；任何异常路径（Redis 故障等）的降级策略不得少收费（fail-closed：判定失败视为过期，按全价）。
8. **多实例部署**：有 Redis 时窗口全局一致；无 Redis 时退化为单实例内存窗口（多副本各自计时），此限制记录在 design 中并接受。

## Non-Goals

- 不改变上游真实缓存行为（上游照常命中缓存，本功能只改网关侧计费与对客展示）。
- 不涉及 cache_creation 计费调整。
- 不做按渠道/模型/用户组的差异化配置。
- 不覆盖音频计费路径（PostAudioConsumeQuota 本就不计缓存折扣，天然无关）。
- 首期不覆盖 OpenRouter 渠道（见 Requirements 3 例外；后续如需覆盖须先快照原始 cached_tokens/Cost 供其计费分支使用）。

## Acceptance Criteria

- [ ] 同一指纹带缓存命中的连续请求：第一次 cached_tokens 清零全价计费，随后 N 秒内恢复折扣，窗口过期后的下一次再次全价。
- [ ] 过期请求客户端收到的 usage：OpenAI 格式 `cached_tokens=0` 且 `prompt_tokens` 不变；Claude 格式 `cache_read_input_tokens=0` 且 `input_tokens` 增加原缓存量。流式（最终 usage chunk / message_delta）与非流式均生效。
- [ ] 过期请求的计费日志 cache_tokens=0，quota 等于全价输入的计算结果；同请求计费侧与响应侧判定一致。
- [ ] 未过期请求：计费与响应与功能关闭时完全一致。
- [ ] cached_tokens=0 的请求不产生任何 Redis/内存查询以外的行为变化（不误开窗口）。
- [ ] 设置为 0 时功能完全关闭，行为与未部署该功能一致。
- [ ] 并发同指纹请求在窗口边界只有一个按全价计费（原子判定，无双扣）。
- [ ] Claude 语义 legacy 派生（Claude 渠道走 OpenAI 格式）路径计费正确，不漏计缓存部分。
- [ ] Gemini 原生格式：过期请求响应中 `usageMetadata.cachedContentTokenCount=0`、`promptTokenCount` 不变，计费全价。
- [ ] OpenRouter 渠道、passthrough-body 请求、音频模型流式：计费与响应均与功能关闭时完全一致（整体跳过）。
- [ ] 格式转换路径（chat↔responses、Gemini↔Responses）同样生效（re-marshal 路径，struct 修改双侧生效）。
- [ ] 非标准缓存字段（DeepSeek `prompt_cache_hit_tokens`、Moonshot `choices[].usage.cached_tokens`、llama.cpp `timings.cache_n`、顶层 `usage.cached_tokens`）在过期请求的响应字节中同样清零。

---

# 附录 B：原 design.md（逐字保留）

# Design: 请求指纹维度的缓存周期性失效与全价计费

所有 file:line 均经两轮独立代码验证（2026-07-10：5 份并行调研 + 3 份对抗审查）。对抗审查确认的 17 项缺陷已全部修入本版。

## 1. 核心架构：门控实时求值 + 窗口结果 memoize（两层分离）

**判定函数**（新增 `service/cache_force_expiry.go`）：`ShouldForceExpireCache(info *relaycommon.RelayInfo) bool`

判定拆成两层，**不能合并 memoize**（对抗审查确认：合并会让跨渠道重试绕过门控——attempt 1 普通渠道判 true 后重试落到 OpenRouter，memoized true 会触发 §3(d) 论证过的错账；反向场景 attempt 1 OpenRouter memoize false 则永久跳过）：

- **门控层（每次调用实时求值，不读写 memoize）**：
  - `interval > 0`（设置项）
  - `info.RequestFingerprint != ""`
  - `info.ChannelType != constant.ChannelTypeOpenRouter`（per-attempt 属性，重试可变）
  - `!info.IsPassThroughBody()`——新增 RelayInfo 方法，落点 relaycommon（该检查只依赖 `model_setting.GetGlobalSettings().PassThroughRequestEnabled` 与 `info.ChannelSetting.PassThroughBodyEnabled`，relay_info.go 已导入 model_setting，无新增依赖边；service 直接导入 relay/channel/claude 会成环，不可字面复用 `shouldSkipClaudeMessageDeltaUsagePatch`——relay-claude.go:18 已导入 service）。claude 包的 shouldSkipClaudeMessageDeltaUsagePatch（relay-claude.go:672-680）同步改为调用此方法。
  - 调用方确认缓存命中信号 > 0（**归一化后**的信号，见 §4 OpenAI 流式行——DeepSeek/Zhipu/Moonshot/llama.cpp 的信号在 `applyUsagePostProcessing`（relay/channel/openai/usage.go:10-52）之后才进入标准字段）
- **窗口层（SetNX 结果 memoize 在 `info.CacheExpiryWindowVerdict *bool`，跨调用与跨渠道重试稳定）**：防止同一请求二次 SetNX 误开二窗；attempt 1 消耗窗口后失败重试，attempt 2 门控通过时复用 memoized true 继续全价，窗口不白耗。

**返回值 = 门控(实时) && 窗口(memoized)**。

**重试重置**：`controller/relay.go` 重试循环（:191-237）每次 attempt 开始时重置 `CacheForceExpired` 与 `OriginalCacheSignals`（每 attempt 的 usage 是新解析对象，但这两个 RelayInfo 字段会跨 attempt 遗留，污染亲和观测还原值与审计文案）。`CacheExpiryWindowVerdict` **不**重置。

**窗口存储**：

- Redis：`RDB.SetNX(ctx, "cache_expiry:{fingerprint}", "1", interval)` 单命令原子。成功=窗口不存在（首见或已过期）→ 全价并开新窗口；失败=窗口内 → 折扣。并发同指纹只有一个成功。仓库无 SetNX wrapper（全仓 grep 零命中），在 `common/redis.go` 新增 `RedisSetNX(key, value string, ttl time.Duration) (bool, error)`（go-redis v8 原生支持）。
- 内存 fallback（`!common.RedisEnabled`）：`sync.Mutex` + `map[string]time.Time(expiry)` + janitor，镜像 `common/rate-limit.go:8-42`。不用 LoadOrStore（过期条目需原子替换）。
- Redis 错误：**fail-closed**——视为过期（全价）+ `logger.LogWarn`。只多收不少收（PRD Req 7）。
- 无 Redis 多实例：各实例独立计时，接受（PRD Req 8）。

## 2. 请求指纹

- 组成：`sha256("{UserId}:{OriginModelName}:{sha256(rawBody)}")`，拼装先例 `buildChannelAffinityCacheKeySuffix`（service/channel_affinity.go:337-349）。
- **哈希原始 body 字节，不重新 Marshal DTO**（非规范化序列化 + `InitChannelMeta` 重试时改 model 名，relay_info.go:244-246）。
- 计算点：`controller/relay.go` `Relay()` 中 `GenRelayInfo`（:120）成功后、预扣费（:161-168）前。门控：interval>0、`!relayInfo.IsChannelTest`、text relay format 白名单（OpenAI/Claude/Gemini/Responses）。不放 `genBaseRelayInfo`（避免 Task/MJ/WS 路径无谓哈希）。
- Body 获取：`common.GetBodyStorage(c)`——Distribute 中间件已缓存（middleware/distributor.go:209），零额外 IO；生命周期覆盖计费时刻（body_cleanup.go:11-22）。**流式哈希**：`Seek(0) → io.Copy(hasher, storage) → Seek(0)`，兼容磁盘存储大 body（body_storage.go:22 IsDisk），新 helper `common.Sha256FromSeeker`（common/hash.go）。
- 存储：`RelayInfo.RequestFingerprint string`（同类先例：RequestId :145、QuotaClamp :169、TieredBillingSnapshot :173）。跨渠道重试稳定（重试循环复用同一 relayInfo）。

## 3. usage 修改语义（正确性核心）

`ForceExpireUsageCache(info, usage *dto.Usage)`，语义判定**镜像 text_quota.go:256 的门**：

```
若 info.OriginalCacheSignals != nil: return   // write-once 守卫：幂等，防二次 P+=CR 叠加
快照（mutation 前）: CachedTokens / InputDetails-CachedTokens / PromptCacheHitTokens
                    / PromptTokens / TotalTokens 五个 int → info.OriginalCacheSignals
claudeStyle := usageSemanticFromUsage(...)=="anthropic" || isLegacyClaudeDerivedOpenAIUsage(...)
if claudeStyle:
    usage.PromptTokens = saturatingAdd(usage.PromptTokens, CR)   // PromptTokens 不含缓存，加回
    usage.TotalTokens  = saturatingAdd(usage.TotalTokens,  CR)   // 保持响应/计费自洽
usage.PromptTokensDetails.CachedTokens = 0
if usage.InputTokensDetails != nil: usage.InputTokensDetails.CachedTokens = 0   // 指针字段，nil 守卫（dto/openai_response.go:235）
usage.PromptCacheHitTokens = 0
info.CacheForceExpired = true
```

- 三个缓存信号字段（dto/openai_response.go:227, 235, 256）必须全清零——亲和观测 `usageCacheSignals`（channel_affinity.go:919-926）依次读这三个。
- **饱和加法是必要护栏，不是双保险**：PromptTokens/CachedTokens 来自**上游响应** usage JSON，`maxTokensLimit` 只约束客户端请求侧字段（valid_request.go:117-120 注释明示），对上游返回值无任何约束（AGENTS.md 明确警告上游数字可声称任意大值）。溢出为负会经 text_quota.go:301 的 quota≤0→1 分支变成上游可触发的少收费。钳制到 `math.MaxInt32`（与 quota 列 32 位及 common/quota_math.go 惯例一致），钳制时 `common.SysError`。
- **禁止**设置 `UsageSemantic`/`UsageSource`：`isLegacyClaudeDerivedOpenAIUsage` 依赖两者为空（text_quota.go:78-80）。
- cache_creation 全字段不动（PRD Req 6）。
- `usageSemanticFromUsage`（text_quota.go:326-334，只看 UsageSemantic 与最终 relay format，不看任何 token 数——清零不会翻转分支）与 `isLegacyClaudeDerivedOpenAIUsage`（:71-82，只看 creation 5m/1h 拆分字段）与 helper 同包，直接复用。

### 计费链路逐分支验证结论（两轮独立推导一致）

| 分支 | 位置 | 结论 |
|---|---|---|
| (a) OpenAI 常规 | text_quota.go:251-260,294 | SAFE：CR=0 → 255-260 整块跳过，baseTokens=全量 PromptTokens 全价 |
| (b) Claude 语义 | :256 门 + :326-334 检测 | SAFE：P+=CR 后走 base 全价；语义检测不看 token 数 |
| (c) legacy Claude 派生 | :71-82, :256 | 计费按 claudeStyle 加回；**响应字节也必须加回 prompt_tokens/total_tokens**（见 §4 OpenAI 行），否则违反 Req 4 |
| (d) OpenRouter Claude | :214-228, quota.go:261-280 | **排除**（门控层实时求值保证重试不绕过）：:219 先减 CR 语义倒挂 + Cost 反推依赖原始 CR |
| (e) 分层计费 | tiered_settle.go:21-49 | SAFE：cr=0 全价；tier 选择输入 Len 两种 mutation 下均不变形 |
| (f) 固定价格 UsePrice | text_quota.go:307-315 | SAFE：公式不含 token 字段 |
| (g) 日志生成 | log_info_generate.go:71-118,271-290 | SAFE：记录 mutation 后值（全价版本，符合 PRD）；审计标记见 §6 |
| (h) 亲和观测 | text_quota.go:341-343, channel_affinity.go:864-875,913-929 | **需快照 5 字段**：观测除三信号外还累加 PromptTokens/TotalTokens（:873-874），Claude 命中率模式 cached/(prompt+cached)（:891-899），只还原三信号会系统性低估命中率 |
| (i) 预扣费 | relay/helper/price.go:93-120 | SAFE：预扣不含缓存折扣；mutation 只增大结算额 |

音频**非流式**计费路径 `PostAudioConsumeQuota`（service/quota.go:282+）不读 CachedTokens——天然 no-op。音频**模型流式**路径见 §5 排除项 3。

## 4. 每格式拦截点

关键事实：多数路径客户端拿到**上游原始字节透传**，改 struct 到不了客户端。**流式 struct mutation 统一钉死在"流结束、最终 usage 确定之后、最终响应构造/返回之前"执行一次**（对抗审查确认的强制约束，见 Claude 行）；事件级只做字节 patch（用同一 memoized 判定）。

**字节 patch 路径清单（与 relay/channel/openai/usage.go 提取器镜像，存在才 patch）**：
`usage.prompt_tokens_details.cached_tokens`、`usage.input_tokens_details.cached_tokens`、`usage.cached_tokens`（顶层，Zhipu 兜底 usage.go:75-77）、`usage.prompt_cache_hit_tokens`（DeepSeek）、`choices.#.usage.cached_tokens`（Moonshot usage.go:86-111）、`timings.cache_n`（llama.cpp usage.go:114-133）。Claude 格式：`usage.cache_read_input_tokens`→0 且 `usage.input_tokens`+=CR。Gemini：`usageMetadata.cachedContentTokenCount`→0。

| 格式 | 非流式 | 流式 |
|---|---|---|
| OpenAI chat（relay-openai.go） | applyUsagePostProcessing（:252，信号归一化）之后判定+struct 修改；透传分支（:271 break→:289 写出）前字节 patch `responseBody`（上表清单 + **claudeStyle 时 `usage.prompt_tokens`/`usage.total_tokens` += CR**——legacy 派生透传若不加回，客户端见 P 而计费 P+CR，违反 Req 4）；re-marshal 分支（usageModified :249 / forceFormat :266 / RelayFormatClaude/Gemini :273-287）struct 自动生效 | **判定前必须先归一化**：`applyUsagePostProcessing` 在最终 chunk 发出（:173）**之后**（:182）才执行——Moonshot/Zhipu/llama.cpp 的信号在 :173 时刻还不在标准字段。hook 在 :173 前对 `lastStreamData` 做等价信号提取（复用 usage.go 提取器）→ 判定 → 字节 patch（上表清单）；struct mutation 在 :182 归一化之后、:184 HandleFinalResponse 之前执行一次（HandleFinalResponse 的 Claude/Gemini 分支会用 usage struct re-marshal 最终事件，helper.go:159-201）。合成 usage 路径（helper.go:153-155）struct mutation 即达客户端。**音频模型流式排除**（§5 例外 3） |
| OpenAI Responses（relay_responses.go） | 透传且 :44 写出在 :47 usage 构建之前——判定用 :29 已 unmarshal 的 `responsesResponse.Usage`；:44 前字节 patch；:47 后 struct 修改（同一 memoized 判定） | patch 条件为**任何携带 `response.usage` 缓存信号的事件**（不只 response.completed——上游存在 response.done/response.incomplete 携带 usage 的终态，chat_via_responses.go:109 佐证），:91 发出前 patch（:86 unmarshal 先于发出，可行）；struct 修改于 :149 返回前 |
| Claude /v1/messages（relay-claude.go） | usage 拷贝（:909-918）后判定；RelayFormatClaude 透传：:936 前字节 patch `responseData` + struct 修改；RelayFormatOpenAI：:923 前 struct 修改一次双侧生效（buildOpenAIStyleUsageFromClaudeUsage :609-627 自动自洽） | **struct mutation 只能在流末执行一次**：`FormatClaudeResponseInfo` 的 message_delta 分支（:751-767）会用上游值**无条件覆盖** `claudeInfo.Usage.PromptTokens/CachedTokens`——在 message_start 处做 struct 修改会被撤销（计费折扣+响应已 patch 全价+窗口已耗=少收费且双侧分歧），P+=CR 也非幂等。故：mutation 放 HandleStreamFinalResponse 内、UsageSemantic 设置（:855）之后、:862 buildOpenAIStyleUsageFromClaudeUsage 与 :893 返回之前（write-once 守卫兜底）。事件级：RelayFormatClaude 对 **message_start 与 message_delta 两种事件**字节 patch（`message.usage.*` / `usage.*`），且**清零 patch 必须在既有 fill-only patch（:815-817 patchClaudeMessageDeltaUsageData）之后执行**——setMessageDeltaUsageInt（:699-712）对"存在但为 0"的字段会用未 mutation 的 claudeInfo.Usage 重新注入原值。RelayFormatOpenAI：流末 struct mutation 已在 :862 前，天然覆盖 |
| Gemini native（relay-gemini-native.go） | :43 usage 构建后判定+struct 修改；:45 写出前字节 patch（promptTokenCount 含缓存、计费按 OpenAI 语义减扣 text_quota.go:256-257，故 promptTokenCount 不动） | 回调内 :85 StringData 前对带 usageMetadata 的 chunk 字节 patch；struct 修改于 geminiStreamHandler 返回（relay-gemini.go:1397）前（:1374 每 chunk 覆盖 `*usage`，故也必须流末执行） |
| **格式转换 handler 家族**（对抗审查补充——原表完全遗漏，功能会静默失效） | 全部 **re-marshal** 路径，struct mutation（于各自 usage 最终确定后、响应构造前）一次双侧生效：`chat_via_responses.go`（OaiResponsesToChatHandler :23 / Buffered :75 / Stream :178，由 compatible_handler.go:74-92 ShouldChatCompletionsUseResponsesGlobal 触发；usage 经 relayconvert/responses_to_chat.go:152 映射缓存字段）、`gemini/relay_responses.go`（GeminiResponsesHandler :21 / Stream :73，gemini/adaptor.go:262-264 分派）、`responses_via_chat.go`（advancedcustom/adaptor.go:234-236 触发） | 同左，流式变体 struct mutation 钉在流末 |

**re-marshal 路径全集**（修正原表头"仅 Claude→OpenAI、Gemini→OpenAI"的不完整概括）：Claude→OpenAI、Gemini→OpenAI、OpenAI→Claude/Gemini（relay-openai.go:273-287）、chat↔responses 双向、Gemini↔Responses、OpenAI 流式合成 usage 路径。这些路径 struct mutation 即达客户端，无需字节 patch。

**渠道收敛**：AWS Bedrock（relay-aws.go:248/279 → claude.HandleClaudeResponseData/HandleStreamResponseData）、Vertex（adaptor.go:331-360 按 RequestMode 分派）、Azure（OpenAI adaptor）全部收敛进上述格式 handler——hook 一次全渠道生效。

## 5. 排除项（已回写 PRD）

1. **OpenRouter 渠道**：§3 表(d)。门控层实时求值保证跨渠道重试不绕过。
2. **passthrough-body 模式**：字节不可改 → 整体跳过（不改计费也不改响应）保双向一致。检查经 `info.IsPassThroughBody()`（落点见 §1）。
3. **音频模型流式**（对抗审查新增）：`isAudioModel` 流式路径的 usage 位于**倒数第二个 chunk**（relay-openai.go:127-131 循环内已发出，:146-162 事后提取）——patch lastStreamData 无效，字节已不可改。整体跳过（双侧），gpt-4o-audio 系列支持 prompt caching 但此路径首期排除。

## 6. 观测与审计

- **亲和观测快照**：`OriginalCacheSignals` 含 **5 个字段**（三信号 + PromptTokens + TotalTokens——observeChannelAffinityUsageCache 除信号外还累加 :873-874，Claude 命中率分母含 prompt）。**write-once**：仅 nil 时写入（ForceExpireUsageCache 的幂等守卫同源）。text_quota.go:341 观测调用点：快照非 nil 时用还原值观测。
- **审计标记**：`info.CacheForceExpired=true` → `PostTextConsumeQuota` extraContent 追加 `"缓存强制过期，本次按全价计费"`。重试时该字段随 attempt 重置（§1）。

## 7. 数据流

```
请求进入 → Distribute 缓存 BodyStorage → GenRelayInfo → 计算 RequestFingerprint（一次）
  → [重试循环，每 attempt 重置 CacheForceExpired/OriginalCacheSignals]
    → 上游响应 → 格式 handler 解析 usage（流式：先归一化缓存信号）
      → 归一化后信号>0? → ShouldForceExpireCache（门控实时 && SetNX memoized）
        → 过期: 事件级字节 patch（透传路径，含 fill-only patch 之后的顺序约束）
                流末/解析完成后 ForceExpireUsageCache 一次（write-once：快照5字段→加回→清零→标记）
        → 未过期: 原样
    → PostTextConsumeQuota（收到已 mutation 的 usage）
      → 亲和观测（快照还原值）→ calculateTextQuotaSummary（零改动，天然全价）
      → 结算 → 日志（cache_tokens=0 + 审计标记）
```

## 8. 影响范围

- `common/hash.go` — `Sha256FromSeeker`
- `common/redis.go` — `RedisSetNX`
- `setting/operation_setting/cache_expiry_setting.go` — 新文件（checkin_setting.go 模式）
- `relay/common/relay_info.go` — RelayInfo 新增 4 字段（RequestFingerprint / CacheExpiryWindowVerdict / CacheForceExpired / OriginalCacheSignals）+ `IsPassThroughBody()` 方法
- `controller/relay.go` — 指纹计算 + 重试循环重置两字段
- `service/cache_force_expiry.go` — 新文件：两层判定 + 窗口存储 + mutation + 快照
- `relay/channel/openai/relay-openai.go`、`relay_responses.go`、`chat_via_responses.go`、`responses_via_chat.go` — hook
- `relay/channel/claude/relay-claude.go` — 非流式/流式 hook + message_start patch + 流末 mutation + shouldSkip 改用共享方法
- `relay/channel/gemini/relay-gemini-native.go`、`relay_responses.go`（+relay-gemini.go 流式返回点）— hook
- `service/text_quota.go` — 亲和观测快照分支 + 审计 extraContent（**计费公式零改动**）
- **不改**：billing 计算、预扣费、结算、渠道 adaptor、DTO 定义

## 9. 设置项

`setting/operation_setting/cache_expiry_setting.go`，`config.GlobalConfig.Register("cache_expiry_setting", ...)`（checkin_setting.go:19-22 模式）。字段 `IntervalSeconds int`，默认 300，option 键 `cache_expiry_setting.interval_seconds`。访问器 clamp 负值→0（GetUsdToCurrencyRate 防御风格，general_setting.go:76-77）。注册后 `PUT /api/option/` 即可配置并持久化（controller/option.go:335），**无需前端改动**。热更新/多节点同步由通用机制承担。

## 10. 风险与已知限制

- 无 Redis 多实例：窗口各实例独立（只多收不少收，方向安全）。
- 字节 patch 改变上游原始响应字节——功能本质要求；sjson 只改目标路径。
- **OpenAI 流式中间 chunk 残留（已知限制，接受）**：若上游在非最终 chunk 携带累计 usage，这些 chunk 经一格延迟发送路径（relay-openai.go:126-131）原样透传，客户端中途可见未清零缓存字段；计费不受影响（只取最终 usage）。逐 chunk gjson 探测成本不值得。
- Responses 流式非终态事件若携带 usage 且未被 patch 条件命中：计费侧此时走估算，不少收。
- 指纹是字节级匹配：任一字节不同即视为不同请求——保守方向，最多多给一个窗口的折扣，不误伤。
- 音频模型流式排除：该路径请求保持原折扣行为。

---

# 附录 C：原 implement.md（逐字保留）

# Implementation Plan

依赖顺序执行；每步验证命令通过后进入下一步。行号基于 2026-07-10 工作区，实现时以符号定位为准。
**两条铁律**（对抗审查确认，违反即产生错账）：
1. **流式 struct mutation 只在流末执行一次**（write-once 守卫）；事件级只做字节 patch。
2. **门控实时求值、窗口结果 memoize** 两层分离；重试每 attempt 重置 CacheForceExpired/OriginalCacheSignals。

## Phase A — 基础原语（无行为变化，可独立合并）

- [ ] A1. `common/hash.go`：`Sha256FromSeeker(r io.ReadSeeker) (string, error)`——`Seek(0)` → `io.Copy` → `Seek(0)` 复位。
- [ ] A2. `common/redis.go`：`RedisSetNX(key, value string, ttl time.Duration) (bool, error)`（`RDB.SetNX`，RedisSet :64 旁）。
- [ ] A3. `setting/operation_setting/cache_expiry_setting.go`：仿 `checkin_setting.go`——`IntervalSeconds int`，默认 300，注册 `"cache_expiry_setting"`，访问器 clamp 负值→0。
- [ ] A4. 测试：Sha256FromSeeker（哈希+Seek 复位）；设置项 clamp。
- 验证：`go build ./... && go test ./common/... ./setting/...`

## Phase B — 指纹、判定核心、共享检查

- [ ] B1. `relay/common/relay_info.go`：
  - RelayInfo 新增字段：`RequestFingerprint string`、`CacheExpiryWindowVerdict *bool`（仅 SetNX 结果）、`CacheForceExpired bool`、`OriginalCacheSignals *CacheSignalsSnapshot`。
  - `CacheSignalsSnapshot` 定义在 relaycommon：**5 个 int**——CachedTokens / InputDetailsCachedTokens / PromptCacheHitTokens / **PromptTokens / TotalTokens**（亲和观测在信号外还累加后两者，channel_affinity.go:873-874）。
  - 新增方法 `IsPassThroughBody() bool`：`model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled`（relay_info.go 已导入 model_setting，无环）。`relay/channel/claude/relay-claude.go` 的 `shouldSkipClaudeMessageDeltaUsagePatch`（:672-680）改为调用它（service 不能导入 claude 包——relay-claude.go:18 已导入 service，反向成环）。
- [ ] B2. `controller/relay.go`：
  - `GenRelayInfo`（:120-124）后插入指纹计算——门控 `interval>0 && !IsChannelTest && relayFormat ∈ {OpenAI, Claude, Gemini, Responses}`；`GetBodyStorage` → `Sha256FromSeeker` → `sha256("{UserId}:{OriginModelName}:{bodyHash}")`。Body 取不到→指纹留空（功能自动跳过），不报错。
  - **重试循环（:191-237）每次 attempt 开始重置 `CacheForceExpired=false`、`OriginalCacheSignals=nil`**；`CacheExpiryWindowVerdict` 不重置（窗口已消耗，跨 attempt 复用防二次开窗/白耗窗口）。
- [ ] B3. `service/cache_force_expiry.go`：
  - `ShouldForceExpireCache(info) bool`：**门控每次调用实时求值**（interval>0、指纹非空、`ChannelType != ChannelTypeOpenRouter`、`!info.IsPassThroughBody()`），任一不过 → false 且**不读写 memoize**；门控通过后：`CacheExpiryWindowVerdict != nil` → 返回缓存值；否则 SetNX（Redis err→fail-closed true+LogWarn；无 Redis→mutex map+janitor，镜像 rate-limit.go:8-42）并 memoize。
  - `ForceExpireUsageCache(info, usage *dto.Usage)`：**write-once**——`info.OriginalCacheSignals != nil` 直接 return（幂等，防 P+=CR 叠加）。顺序：①快照 5 字段 ②`claudeStyle := usageSemanticFromUsage(...)=="anthropic" || isLegacyClaudeDerivedOpenAIUsage(...)`（text_quota.go:256 同门，同包复用）→ claudeStyle 时 `PromptTokens`/`TotalTokens` 饱和加 CR（**上游控制值，无前置约束**——maxTokensLimit 只管请求侧；钳制 math.MaxInt32 + `common.SysError`）③清零三信号（`InputTokensDetails` 是 `*InputTokenDetails` 指针，**nil 守卫**，dto/openai_response.go:235）④`CacheForceExpired=true`。**不写 UsageSemantic/UsageSource；不动 cache_creation 字段。**
  - 字节 patch 工具函数（sjson，供 Phase C 复用）：OpenAI 清单——`usage.prompt_tokens_details.cached_tokens`、`usage.input_tokens_details.cached_tokens`、`usage.cached_tokens`、`usage.prompt_cache_hit_tokens`、`choices.#.usage.cached_tokens`、`timings.cache_n`（存在才 patch；claudeStyle 时另 `usage.prompt_tokens`/`usage.total_tokens` += CR）；Claude 清单——`cache_read_input_tokens`→0、`input_tokens`+=CR；Gemini——`usageMetadata.cachedContentTokenCount`→0。
- [ ] B4. 测试 `service/cache_force_expiry_test.go`：
  - 窗口：首见 true / 窗口内 false / 过期后 true / interval=0 false / 内存并发（两 goroutine 仅一 true）。
  - **门控实时性**：verdict memoized true 后把 info.ChannelType 改为 OpenRouter → 返回 false（重试场景）；passthrough 双开关（全局 PassThroughRequestEnabled、渠道 PassThroughBodyEnabled）各自 → false。
  - **cached=0 不开窗**：无缓存信号时任何 hook 不写 SetNX/内存 map（AC5）。
  - mutation 表测试：OpenAI 语义（只清零，P/Total 不变）/ Claude 语义（P、Total 各 +CR 一次）/ legacy 派生（5m>0、语义空 → 加回）/ **重复调用幂等（第二次 no-op，快照不被污染）** / cache_creation 不动 / 三信号全清零 + InputTokensDetails nil 不 panic / 快照 5 字段正确 / 上游 MaxInt64 级 token 饱和钳制。
  - 计费整合：mutation 后 usage 喂 `calculateTextQuotaSummary`，OpenAI 与 Claude 语义下 quota 均等于全价输入。
- 验证：`go build ./... && go test ./service/... -run 'CacheForceExpiry|TextQuota'`

## Phase C — 响应侧 hook（按格式独立验证）

统一模式：**事件级只做"归一化后信号>0 → ShouldForceExpireCache → 字节 patch"**；**struct mutation 一律在该 handler 的 usage 最终确定后、最终响应构造/返回前执行一次**（write-once 兜底）。

- [ ] C1. OpenAI chat `relay/channel/openai/relay-openai.go`：
  - 非流式 `OpenaiHandler`：applyUsagePostProcessing（:252）后判定+struct 修改；透传分支 :289 写出前字节 patch（B3 完整清单，含 claudeStyle 的 prompt_tokens/total_tokens 加回）；re-marshal 分支（usageModified/forceFormat/RelayFormatClaude/Gemini :273-287）struct 自动生效。
  - 流式 `OaiStreamHandler`：**判定前先归一化**——:173 发出前对 `lastStreamData` 执行与 applyUsagePostProcessing 等价的信号提取（复用 usage.go:10-52 提取器；否则 DeepSeek/Zhipu/Moonshot/llama.cpp 流式信号在 :173 时刻不可见，功能静默失效或双侧分歧）→ 判定 → 字节 patch；struct mutation 在 :182 之后、:184 HandleFinalResponse **之前**（其 Claude/Gemini 分支用 usage struct re-marshal 最终事件，helper.go:159-201）。合成 usage 路径仅 struct mutation。**`isAudioModel` 流式整体跳过**（usage 在已发出的倒数第二 chunk，:127-131/:146-162，字节不可改——双侧跳过）。
- [ ] C2. Responses `relay/channel/openai/relay_responses.go`：
  - 非流式：:29 unmarshal 后判定；:44 写出前字节 patch；:47 后 struct 修改。
  - 流式：patch 条件 = **事件含 `response.usage` 缓存信号**（不限 response.completed——response.done/incomplete 也可携带 usage，chat_via_responses.go:109 佐证），:91 发出前 patch；:149 返回前 struct 修改。
- [ ] C3. Claude `relay/channel/claude/relay-claude.go`：
  - 非流式 `HandleClaudeResponseData`：usage 拷贝（:909-918）后判定；RelayFormatClaude → :936 前字节 patch + struct 修改；RelayFormatOpenAI → :923 前 struct 修改。
  - 流式（**铁律 1 的主战场**）：判定在 message_start 首见信号时做（memoized）；事件级只字节 patch——RelayFormatClaude 对 **message_start 与 message_delta 两种事件** patch，且**清零 patch 必须排在既有 fill-only patch（:815-817 patchClaudeMessageDeltaUsageData）之后**（setMessageDeltaUsageInt :699-712 对"存在但为 0"的字段会用未 mutation 的 claudeInfo.Usage 注回原值）；**struct mutation 唯一执行点：HandleStreamFinalResponse 内、UsageSemantic 设置（:855）后、:862/:893 前**——message_delta 解析（:751-767）会无条件覆盖 PromptTokens/CachedTokens，在 message_start 处改 struct 必被撤销（少收费+双侧分歧）。RelayFormatOpenAI：流末 mutation 已天然覆盖 :862 的 buildOpenAIStyleUsageFromClaudeUsage。
- [ ] C4. Gemini：
  - 非流式 `GeminiTextGenerationHandler`（relay-gemini-native.go:20）：:43 后判定+struct 修改；:45 前字节 patch。
  - 流式：回调内 :85 前对带 usageMetadata 的 chunk 字节 patch；struct mutation 于 geminiStreamHandler 返回（relay-gemini.go:1397）前（:1374 每 chunk 覆盖 `*usage`，同样必须流末）。Gemini→OpenAI 转换路径仅 struct mutation。
- [ ] C5. **格式转换 handler 家族**（对抗审查补充，原计划遗漏——这些路径缓存字段照常暴露，不 hook 则功能静默失效）：全部 re-marshal，仅需 struct mutation（各自 usage 最终确定后）：
  - `chat_via_responses.go`：OaiResponsesToChatHandler（:23）/ Buffered（:75）/ Stream（:178）——compatible_handler.go:74-92 ShouldChatCompletionsUseResponsesGlobal 触发，usage 经 relayconvert/responses_to_chat.go:152 映射。
  - `gemini/relay_responses.go`：GeminiResponsesHandler（:21）/ Stream（:73）。
  - `responses_via_chat.go`（advancedcustom/adaptor.go:234-236 触发）。
- [ ] C6. 测试（每格式，仿 message_delta_usage_patch_test.go 风格）：
  - 过期判定下：patch 后字节缓存字段为 0、输入字段正确、其余字节不变；未过期：字节完全不变。
  - **Claude 流式回归**：message_start 带缓存 + message_delta 重报 input_tokens/cache_read → 最终计费 usage 仍全价且 P 只加一次 CR；上游 message_delta 缺 cache 字段（Bedrock 场景）+ 已过期 → fill-only 补全不把缓存值带回。
  - **legacy 派生透传**：带 claude_cache_creation_5_m_tokens、无 usage_semantic 的上游 JSON → patch 后 prompt_tokens/total_tokens 已加回，计费=响应展示。
  - **DeepSeek 风格流式**（prompt_cache_hit_tokens）：归一化后判定触发、patch 生效。
  - passthrough 请求：usage 与字节均不变（AC10）。
- 验证：`go build ./... && go test ./relay/channel/... -run 'CacheExpiry|UsagePatch'`

## Phase D — 计费侧收尾

- [ ] D1. `service/text_quota.go`：
  - :341-343 亲和观测：`OriginalCacheSignals != nil` 时用还原值观测（**5 字段全还原**：三信号+PromptTokens+TotalTokens——观测累加 :873-874 且 Claude 命中率分母含 prompt :891-899）。
  - `CacheForceExpired` → extraContent 追加 `"缓存强制过期，本次按全价计费"`。
  - **calculateTextQuotaSummary 及所有计费公式零改动。**
- [ ] D2. 回归测试：观测收到的 prompt/total/cached 与未 mutation 时完全一致；审计文案出现；全链路——同指纹两次请求（mock 窗口），第一次 quota=全价+日志 cache_tokens=0，第二次 quota=折扣+cache_tokens 原值；重试场景——attempt 1 判 true 后换 OpenRouter 渠道，mutation 不执行、计费与功能关闭一致。
- 验证：`go build ./... && go test ./service/... ./relay/... ./common/...`

## Phase E — 收尾

- [ ] E1. `go build ./... && go test ./...`（无 DB 变更，三数据库兼容性不涉及）。
- [ ] E2. 手动验证：非流式+流式带缓存命中请求（Claude 与 OpenAI 各一）——①客户端响应 usage ②管理端日志 cache_tokens/quota ③窗口内第二次恢复折扣 ④Redis key `cache_expiry:*` TTL。
- [ ] E3. `PUT /api/option/` 设 `cache_expiry_setting.interval_seconds=0` 验证整体关闭。

## 回滚点

- Phase A/B 纯新增，回滚=删文件+删字段。
- Phase C 每格式独立，可单格式回滚。
- 运行时紧急关闭：`interval_seconds=0`（热更新，无需重启）。

## Validation

```bash
go build ./...
go test ./common/... ./setting/... -run 'Sha256FromSeeker|CacheExpiry'
go test ./service/... -run 'CacheForceExpiry|TextQuota'
go test ./relay/channel/... -run 'CacheExpiry|UsagePatch'
go test ./...
```
