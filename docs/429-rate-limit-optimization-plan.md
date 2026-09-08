# 429/限流处理链路全面审查与优化方案

> 基于 2026-09-08 线上事故（北京时间 09:45–09:59，账号 `f205e3da` 被 72 次重试打穿）的代码审查。
> 审查范围：`internal/web/server.go`（路由与 failover）、`session_resolver.go`、`account_health.go`、`account_concurrency.go`、`errors.go`、`stream.go`、`internal/chathub/client.go`（metering 判定）、`connpool.go`。

> **实现状态（2026-09-08 第二轮）**：阶段 1/2/3/4 全部落地并通过 `go build/vet/test`，见下表。
>
> | 方案项 | 状态 | 代码位置 |
> |---|---|---|
> | 1.1 限流账号本地快速失败 | ✅ | `account_health.go RemainingCooldown` + `throttle_guard.go maybeShortCircuitThrottle/writeLocalThrottle`，openaiChat/chatStream resolveAccount 后短路，响应带精确 Retry-After 与 `X-M365-Cooldown-Until` |
> | 1.2 metering 耗尽冷却至 UTC midnight | ✅ | `account_health.go chatQuotaExhausted` + MarkFailure QUOTA_429 分支（仅 LLMOnly remaining≤0 判定耗尽） |
> | 1.3 router failover 上限 | ✅ | 两条 router failover 循环加 `FailoverMaxAttempts`（settings，默认 3，env `M365_FAILOVER_MAX_ATTEMPTS`） |
> | 2.1 ChatAvailable | ✅ | `account_health.go ChatAvailable`（LLMOnly 估计耗尽跳过；跨 UTC 天自动失效重置）；接入 resolveAccount 轮询/偏好与 nextHealthyAccountExcluding |
> | 2.2 resolver 解绑不健康账号 | ✅ | openaiChat 中 sessionKey / user-session / session-resolver 三个提示源命中 `!accountAvailable` 时解绑；resolver 解绑时恢复全量 prompt+attachments |
> | 2.3 pin 语义细化 | ✅ | `X-M365-Cooldown-Until` 响应头；`X-M365-Allow-Failover: true/1` 请求头解除 pin 的 failover 否决（首 attempt 仍打 pin 账号，冷却闸门处转为 failover 而非本地 429，见 `throttle_guard.go handleCooldownGate`，chatOnce/openaiChat/chatStream 三路径接入） |
> | 3.1 router prompt 自适应瘦身 | ✅ | `router_prompt.go buildRoutePrompt`：system+最近 N 条（默认 4/条窗 4KB）、指代特征词自动升级全量、小 payload 不动、`M365_ROUTER_PROMPT_SLIMMING=false` 一键回退 |
> | 3.2 请求指纹防抖 | ✅ | `throttle_guard.go requestDebounce`：30s 窗口回放 429/5xx，带 `X-M365-Cached-Error`；TTL 不随回放刷新（保留恢复探测） |
> | 3.3 429 响应信息增强 | ◑ 部分 | 短路响应带 cooldown_until；普通 429 未改 |
> | 4.1 结构化限流日志 + 事件记录 | ✅ | 指标注册表 `throttle_stats.go`（24h 滚动窗，事件环 200 条）：`RecordThrottleEvent` 埋在 `markAccountResult`（上游真 429）、`writeLocalThrottle`（本地短路）、`debounceReplay`（防抖回放）、`nextHealthyAccountExcluding`（failover）、探活结果 |
> | 4.2 管理台 429 面板 | ✅ | API `GET/DELETE /api/throttle-stats`（admin 会话保护）+ 仪表盘「429 / 限流监控」卡片（四计数 + 最新事件表，15s 自刷新；`web/index.html` 与嵌入副本已同步） |
> | 4.3 冷却到期主动探活 | ✅ | `throttle_probe.go StartCooldownProber`：默认 60s（`M365_PROBE_INTERVAL_SECONDS`，下限 15；`M365_PROBE_ENABLED=false` 关闭）扫描 `ExpiredLimitedCooldowns`，用最小 "ping" 请求走 `chatWithAccount` 探活，成功即清限 + 记 `[probe] recovered` + 删一次性云对话；熔断开启时跳过，每 tick 上限 8 个、同账号防并发 |
> | 单测 | ✅ | `throttle_guard_test.go`：耗尽判定、UTC midnight 冷却、短路响应、ChatAvailable 跨天恢复、轮询跳过耗尽账号、防抖 TTL、路由窗孤儿 tool、瘦身/指代升级、默认配置、allow-failover 头解析、冷却闸门四分支、探活候选快照不清理、指标记录/排序/环形上限/重置 |

---

## 一、现状链路（事实梳理）

```
客户端请求（27 条消息，93KB prompt，31 tools）
  │
  ├─ openaiChat (server.go:1721)
  │    clientPinnedAccount := body.AccountID != ""        (server.go:1764)
  │    session-resolver 内容前缀匹配 → body.AccountID = 上次账号  (server.go:1850-1856)
  │    acc := s.resolveAccount(accountID)                  (server.go:1869)
  │
  ├─ resolveAccount(accountID)                             (server.go:1078)
  │    accountID == ""  → lastHealthyAccount 优先 → 轮询跳过不健康账号 ✅ 有健康检查
  │    accountID != ""  → EnsureValid(accountID)           ❌ 只验证 token，完全不看冷却/限流状态
  │
  ├─ tool-router（planningMode=="router" 且非流式）
  │    routePrompt = modelToolRouterPrompt(answerPrompt + …)  ← 93KB 全量进路由 (server.go:1960)
  │    chatWithAccount → Chat() → 上游 type:2 帧 result.value="Throttled"
  │      → MeteringError{Cause: ErrMeteringThrottled}       (client.go:1226-1232)
  │    recordAccountResultForCapability → MarkFailure(QUOTA_429)
  │      → limited=true + 指数冷却 30s→30min (quotaAttempts) (account_health.go:877-885)
  │    failover 循环: !clientPinnedAccount && IsRateLimited → nextHealthyAccountExcluding
  │                                                          (server.go:1972-1982)
  │    全部失败 → "[tool-router] account=… failed" → 客户端 429 (server.go:1985)
  │
  └─ 客户端每 3s~40s 指数退避重试 → 每次重试都是一个新 HTTP 请求，
       重新走 session-resolver 匹配 → 重新钉回同一个账号 → 重新打上游
```

**线上证据**（journalctl，北京时间 09:45–09:59）：72 次 429，每次都有
`[connpool] hit` + `chathub prompt-trace text=93237` + `[chathub] result.value="Throttled"`，
即**网关明知账号已被限流，每次客户端重试仍原样转发 100KB payload 打上游**。

---

## 二、问题清单（按严重度）

### P0-1 已知限流的账号仍被反复打上游 —— 重试放大器

- `resolveAccount(accountID)` 在 accountID 非空（客户端 pin / resolver pin / user-session / conv-cache）时**跳过全部健康检查**（server.go:1078，仅 1118 行 EnsureValid）。
- `accountHealth.limited/cooldown` 状态已经存在且被正确写入（MarkFailure → QUOTA_429 分支），但**读路径没有利用**：pinned 账号在冷却期内，网关仍然发起真实上游调用。
- 后果：客户端 72 次重试 = 72 次上游计量调用，配额越打越死，还拉长上游侧的限流窗口。

### P0-2 session-resolver 把会话钉死在不健康账号上

- Resolve 返回的 `AccountID` 被直接赋给 `body.AccountID`（server.go:1856），随后走无健康检查的 resolveAccount 分支。
- 会话绑定只记录「哪个账号」，没有「账号是否健康」维度。账号进入冷却后，该会话的所有后续请求继续撞墙。
- 答案轮 failover 其实已有成熟先例（server.go:2088-2099：换号时清 `failoverReq.ConversationID` 全量重发），但 resolver 钉住的请求到不了这条路径。

### P0-3 clientPinnedAccount 一票否决所有 failover 与健康干预

- 客户端在 body/header 里带 accountId 时（本次事故客户端即如此），`!clientPinnedAccount` 使 router failover（server.go:1972）与答案轮 failover（server.go:2088/2399/2504）全部短路。
- 「尊重客户端 pin」合理（否则客户端看不到真实拒绝原因），但 pin 的正确语义应该是：**第一次照打、失败后快速失败**，而不是每次重试都替客户端消耗一次真实上游配额。

### P1-4 tool-router 全量 prompt 浪费配额

- 路由决策只需要「最近消息 + 工具列表」，但 `routePrompt` 携带完整 93KB 历史（server.go:1960）；解析失败还会追加一次 repair 上游调用（server.go:1994-1995）。
- 对长对话 agent 场景，每次请求的配额消耗被放大数倍到数十倍，直接加速触发 metering throttle。

### P1-5 QUOTA_429 冷却时长不精细，allowance 数据未参与路由

- `result.value="Throttled"`（配额耗尽）与短窗 burst throttle 都走同一套 30s→30min 指数退避（account_health.go:263-283, 877-885）。
- `remainingAllowances(throttling)`（stream.go:210）已能解析 `throttling.metering.<capability>.remainingAllowance`，`UpdateMetering/RecordAllowanceConsumption/GetAllowanceSnapshot` 基建齐全（account_health.go:674-746），但**只用于管理台展示，不参与账号选择**。
- 图片侧已有按 capability 隔离冷却的先例（`ImageGenAvailable` + `nextImageAccount`），chat 侧没有对应的 `ChatAvailable`。

### P1-6 quotaAttempts 重置过快，退避强度随重置归零

- 冷却到期即删 `quotaAttempts`（account_health.go:502），下次 429 又从 30s 起步。对「配额见底」型账号，退避实际上永远到不了有意义的时长。
- MarkSuccess 也无条件清 `quotaAttempts`（account_health.go:944）。

### P2-7 无重试风暴防护（请求级防抖）

- 同一客户端 30s 内重发同一请求（同 tenant+会话+末条消息 hash），网关每次都完整走一遍上游调用。`agent_ledger` 只做工具调用去重，不覆盖此场景。
- 本次事故 72 连击，若有指纹级短路，71 次可以在本地直接返回。

### P2-8 429 可观测性不足

- `[upstream-fail]` 不带 requestID/conversation/prompt_len/route 阶段，排查必须靠多条日志手工关联。
- `result.value=Throttled` 时若 `throttling.metering` 携带 remainingAllowance，日志未输出，无法判断账号水位。

### P2-9 resolveAccount 轮询探测上限 16（maxAccountProbe）

- 当前账号池 33 个账号 > 16。环形游标（tokens.Next）保证长期公平，但「连续 17 个不健康账号 + 短时间高频请求」时会提前失败。低优先级，随账号池扩张需改为按 `len(accounts)` 探测。

### P2-10 WS_PROTOCOL/502 噪音（旁路问题）

- 7 天内 196 次 502 InternalError、30+ 次 WS_PROTOCOL（RSV2 set 等坏帧）。connpool 已做坏连接丢弃，不影响本次主线，列入监控。

---

## 三、优化方案（分四阶段，可独立上线）

### 阶段 1：止血（改动最小，直接消掉本次事故形态）

**1.1 限流账号本地快速失败（核心）**
- 新增 `accountHealth.RemainingCooldown(accountID) (time.Duration, bool)`。
- `openaiChat`/`chatOnce` 在 `resolveAccount` 返回后：若该账号 `limited && RemainingCooldown > 0`，**不发起上游调用**，直接 `writeUpstreamErrorWithAccount`（本地构造 UpstreamHTTPError{Status:429, RetryAfter:剩余秒数}），日志记 `[throttle-shortcircuit]`。
- 客户端 pin、resolver pin 一视同仁——pin 尊重的是「用这个账号」，不是「替我把上游打穿」。
- 效果：72 次上游调用 → 1 次真实探测 + 71 次本地响应（每次带精确 Retry-After）。

**1.2 metering 耗尽型 429 的冷却与配额水位挂钩**
- `MarkFailure` 收到 `*chathub.MeteringError` 时，用 `remainingAllowances(err.Throttling)` 判断目标 capability：`remainingAllowance == 0` → 冷却直接设到 `nextUTCMidnight()`（与图片侧同构），而不是 30s 指数。
- 无法判定水位时维持现有指数退避，行为不变。

**1.3 router failover 循环加上限**
- `for !clientPinnedAccount && …` 增加显式 attempt 计数（建议 ≤3），避免账号池大面积异常时循环拖满请求超时。

### 阶段 2：路由感知配额与健康

**2.1 ChatAvailable（capability 级账号选择）**
- 仿照 `ImageGenAvailable`：基于 `GetAllowanceSnapshot` 的 estimated `LLMOnly`（或实际请求 capability），`estimated <= 0` 的账号在 `resolveAccount("")` 轮询与 `nextHealthyAccountExcluding` 中跳过。
- allowance 数据来源：`UpdateMetering`（已有）+ 每次成功调用后 `RecordAllowanceConsumption`（已有），无需新增上游探测。

**2.2 session-resolver 解除对不健康账号的钉死**
- openaiChat 中 resolver 命中后（server.go:1856）：
  ```go
  if !clientPinnedAccount && resolved.AccountID != "" && !s.accountAvailable(resolved.AccountID) {
      log.Printf("[session-resolver] unpin throttled account=%s conversation=%s", …)
      body.AccountID = ""          // 走健康轮询
      body.ConversationID = ""     // 换号必须弃云对话，答案轮 failover 已有全量重发逻辑
  }
  ```
- 冷却结束后，下次请求 context 前缀匹配自动回归原账号原对话（Bind 逻辑不变），上下文连续性自然恢复。

**2.3 客户端 pin 的语义细化**
- pin + 限流 = 快速失败（1.1 已覆盖），响应头带 `X-M365-Cooldown-Until`。
- 可选：支持请求头 `X-M365-Allow-Failover: true`，授权网关在 pin 账号 429 时替客户端换号（默认保持现状语义）。

### 阶段 3：降本与防风暴

**3.1 router prompt 自适应瘦身**

先澄清架构事实：路由调用（server.go:1960-2016）与回答调用（server.go:2018-2064）是两个独立上游请求。路由输出仅为 JSON 工具决策，经校验后以 tool_calls 返回（server.go:2014），从不生成用户可见回答；回答由 answer turn 生成，prompt 为完整历史（或云对话增量），**瘦身不触碰回答路径**。换号时现有逻辑即全量重发（server.go:2098-2102），新账号不丢历史；路由跑在用完即删的一次性云对话中，不承载记忆。因此瘦身只影响「工具选择是否准确」，不影响回答质量与上下文完整性。

真实风险是路由决策依赖历史：多步 agent 流的指代类输入（"接着用刚才那个工具"）、依赖早期工具结果的决策。失败模式 = 选错/漏选 tool_calls 或触发 repair 重试。为此采用自适应设计而非一刀切：

- 基线构成：最近 N 条消息（`M365_ROUTER_PROMPT_TAIL_MESSAGES`，默认 4）+ 每条截断（`M365_ROUTER_PROMPT_MAX_BYTES`，默认 4KB）+ `ledger.RouterContext()`（工具执行摘要，本就是为路由设计）+ 执行锚点。
- 自动升级为全量的触发条件（任一满足）：最近消息含指代/引用特征（"上面/之前/刚才/继续/同样"等启发式词表）；上一轮路由进入 repair 或解析失败；上一轮路由结果为空。
- 灰度观测：对比瘦身前后的路由命中率（`parsed && len(calls)>0` 比例）、repair 触发率、tool_calls 名称分布，三项不劣化才全量推开；配置开关一键回退全量。
- 权衡说明：不瘦身的隐性代价是每次请求多消耗数倍计量配额，更频繁触发 metering throttle——而 throttle 一旦触发，整个会话连回答都发不出（本次事故即如此）。「路由决策偶尔次优（且有自动升级兜底）」优于「整个账号被限流」。

**3.2 请求指纹级防抖**
- key = sha256(tenant + conversation + 末条消息内容 + model)，30s 滑窗内重复且上一次结果是 429/5xx → 直接回放上次错误（加 `X-M365-Cached-Error: true` 头区分）。
- 内存占用有限（LRU 上限即可），不影响正常并发。

**3.3 429 响应信息增强**
- message 附带 `cooldownUntil` 与账号标识（oid 后 4 位），`X-M365-RateLimit-Remaining` 使用真实 allowance 估计值而非 0/1。

### 阶段 4：可观测性与长期演进

**4.1 结构化限流日志**
- `[upstream-fail]` 与 `[throttle-shortcircuit]` 增加：requestID、conversation、route_stage(router/answer/repair)、prompt_len、attempt、cooldown_until、remaining_allowance。一次 grep 即可还原事故全貌。

**4.2 管理台 429 面板**
- 按账号 × 小时聚合 429/502/成功数与 allowance 水位曲线（数据源：accountHealth.Snapshot 已含大部分字段）。

**4.3 allowance 水位探针（对应 docs/har-mining 07-errors-risk.md 的 P0 建议）**
- 每账号冷却结束后的第一次成功响应，解析并记录 meteringInformation.remainingAllowance；水位低于阈值（如 20%）时降低该账号调度权重，实现「提前避让」而非「撞墙后退避」。

---

## 四、验证方案

| 层级 | 用例 |
|---|---|
| 单测 | pinned+limited 账号快速失败不触上游；resolver 命中限流账号时解绑换号；MeteringError remaining=0 → 冷却至 UTC midnight；router prompt 长度上限 |
| 集成回放 | 用本次事故特征（27 消息 93KB + 3s 间隔 72 次重试）回放，断言上游调用次数 == 1、其余为本地 429 且 Retry-After 单调一致 |
| 上线观察 | `[upstream-fail] status=429` 与 `result.value=Throttled` 频次应显著下降；`[throttle-shortcircuit]` 出现即说明短路生效；账号间调用分布（calls 字段）是否更均匀 |

## 五、风险与权衡

- **快速失败 vs 上游提前恢复**：metering 恢复无通知，冷却期内账号可能已可用。缓解：保留「冷却到期后第一请求真实探测」现状；可选每 5 分钟对 pinned 场景放行一次探测请求。
- **换号丢云上下文**：resolver 解绑后全量重发，正确性由现有 failover 路径保证（server.go:2099 已处理），代价是多一次全量 token——对比事故中 72 次全量，净收益为正。
- **router prompt 瘦身的决策质量**：指代类输入（「就用上面第一个工具」）可能误判。缓解：自适应升级机制（指代特征/上一轮 repair/空结果时自动回全量）+ 灰度期对比路由命中率与 repair 率 + 一键回退开关。

---

## 六、对标 new-api：可借鉴与不可照搬

> 结论先行：**不能"全面学习"——两者上游协议与配额模型有本质差异；但 new-api 有四件成熟实现值得直接抄**，且其中两件正是本次事故缺的防线。

### 6.1 new-api 的相关机制（源码核实）

| 机制 | 实现 | 要点 |
|---|---|---|
| 入口多层限流 | `middleware/rate-limit.go`、`middleware/model-rate-limit.go` | 按 IP/User/分组的多级固定窗口（Redis Lua 原子）+ 令牌桶；模型级限流区分「总请求数」与「成功请求数」；429 响应带 Retry-After |
| 渠道选择 | `middleware/distributor.go` → `CacheGetRandomSatisfiedChannel` | 优先级分组 → 同级加权随机；客户端指定渠道（specific_channel_id）时不重试 |
| 重试决策 | `controller/relay.go` `shouldRetry()` | RetryTimes 可配置；429 重试、5xx 重试、400/408 不重试；记录 `use_channel` 轨迹（"重试1:2-15"） |
| 渠道健康 | `service/channel.go` `ShouldDisableChannel`/`DisableChannel` | 401/403/配额不足 → 自动禁用 + 关键词 AC 自动机匹配 + **通知管理员**；429 → 重试但不禁用 |
| 自动恢复 | `EnableChannel` + 定期渠道测试（channel-test 复用正式管线发最小请求） | 被禁渠道探活成功自动启用并通知 |
| 配额 | pre-consume（预扣）→ post-consume（按实际用量结算）→ 失败退款 | 纯本地记账 |

### 6.2 值得直接借鉴的四件事

1. **入口限流中间件**（对应本文 3.2，new-api 实现最成熟）：per-API-key/per-IP 固定窗口 + 429 + Retry-After。M365 已有 tenant（API key）概念，落地成本低。这是客户端重试风暴的第一道防线——new-api 体系下今早那种 72 连击在网关入口就被拦掉，根本到不了上游选择逻辑。
2. **自动探测恢复**（强化本文 4.3）：M365 冷却到期后被动等第一个真实请求撞墙；学 new-api 的 channel-test 思路，冷却到期时后台用最小请求（1 token）主动探活，探活成功才恢复调度，避免把恢复探测的成本转嫁给真实用户。
3. **failover 轨迹与可配置重试数**：学 `use_channel` 日志——M365 的 failover 有 `triedAccountIDs` 但不输出，应记 `tried=a,b,c`；图片路径的 `maxAccountProbe=16` 与 router 循环无上限（阶段 1.3）统一改为可配置 `M365_FAILOVER_MAX_ATTEMPTS`。
4. **管理员通知**：new-api 渠道禁用/恢复会 NotifyRootUser。M365 应在「可用账号数低于阈值」「单账号短窗内 N 次 429」「401/403 禁用」时推送告警（webhook/Telegram），而不是等人来翻 journalctl。

### 6.3 不可照搬的三个本质差异

1. **上游协议**：new-api 的渠道全是标准 HTTP 请求-响应（OpenAI 格式），重试 = 重发一个无状态请求。M365 Copilot 是 WebSocket 会话 + 流式输出 + 云端对话归属（conversation 绑定账号），failover 必须处理「已流式输出不能换号」「换号丢云对话需全量重发」——本项目的 failover（server.go:2088-2102）在这一点上比 new-api 复杂且正确，照搬它的简单重试会引入重复输出 bug。
2. **配额模型**：new-api 本地记账（预扣/结算/退款），配额完全自持。M365 配额在微软侧（meteringInformation），只能解析 + 估计 + 探针，学习方向是把 pre-consume 思想映射到已有的 `RecordAllowanceConsumption` 预扣上，而非引入本地计费表。
3. **禁用 vs 冷却**：new-api 对 401/403 直接禁用渠道；对 429 不禁用只重试。M365 的按分类冷却（429 指数退避、403 长冷却、401 短冷却后自动复用）其实比「立即禁用」更适合瞬时限流主导的 Copilot 场景，保持现有模型、仅叠加 6.2-2 的主动探活即可。

### 6.4 落地顺序修订

阶段 3.2 升级为：参考 new-api 的 `middleware/rate-limit.go` 固定窗口实现入口限流（请求指纹防抖与其互补：限流管「频率」，指纹防抖管「重复内容」）。阶段 4.3 的探针吸收 new-api 的主动探活与通知机制。其余阶段不受影响。
