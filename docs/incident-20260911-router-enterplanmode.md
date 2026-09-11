# 事故报告：执行请求被路由成 EnterPlanMode —— "只列计划、什么都没做"

- **日期**：2026-09-11（日志窗口 2026-09-10 12:40 → 2026-09-11 01:17 UTC，约 12.5h / 10435 行）
- **现象**：用户在 agent 客户端输入 `实现以上未落地的内容`，模型只输出了一份计划就结束，没有任何工具调用、没有任何文件改动。
- **结论**：**不是模型不听话，是网关的路由轮把这一步决策花在了"进入计划模式"上**。`EnterPlanMode` 是一个只会**中断执行**的元工具，被当成"最有决定性的下一步"选中后，客户端切进计划模式，本轮在协议层就只能产出计划文本。
- **定性**：网关缺陷（路由偏见未覆盖"模式切换类"工具），非上游模型缺陷，非客户端缺陷。
- **状态**：代码已修复 + 回归测试通过（`go build` / `go vet` / `go test ./...` 全绿）；**未部署**（原因见文末"待决"）。

---

## 1. 证据链

### 1.1 网关侧：路由轮选中了 EnterPlanMode

```
2026/09/11 00:56:50 [tool-router] id=38719abd-e528-4119-ac28-c3c11ae5eadf \
  stage=stream-router decision=call count=1 names=[EnterPlanMode] elapsed_ms=7172
2026/09/11 00:56:50 chathub timing completion_frame_ms=7141 streamed_text=28
```

`streamed_text=28` 恰好是 `CALL_TOOL: EnterPlanMode({})` 的长度 —— 路由轮的上游输出就是这一条空参调用。

### 1.2 客户端侧：同一条请求的响应确认了这个调用

客户端会话存档 `~/.zcode/cli/rollout/model-io-sess_f083b481-*.jsonl` 中该记录的 `response.headers.x-request-id`
与上面网关日志的 id **完全一致**（`38719abd-e528-4119-ac28-c3c11ae5eadf`），响应为：

| 字段 | 值 |
|---|---|
| `finishReason` | `tool-calls` |
| `toolCalls` | `[{"name":"EnterPlanMode","input":{}}]` |
| `reasoningText` | `**Choosing EnterPlanMode** — I'm opting for EnterPlanMode as it seems to be the best approach for advancing with the non-trivial implementation, even though the audit has already been completed.` |
| `usage` | `inputTokens=19, outputTokens=18` |

模型的原话就是"**进入计划模式似乎是推进这件事的最佳方式**" —— 这正是路由提示里"选出能推进工作的一步"的措辞反噬。

### 1.3 下一轮：客户端被锁进计划模式

紧接着的请求（00:57:22）携带了客户端注入的 system-reminder：

```
Plan mode is active. The user indicated that they do not want you to execute yet...
```

此刻 agent 在**客户端侧**已被禁止执行，唯一能做的就是写计划 —— 用户看到的现象由此产生。

### 1.4 收尾：网关自己也得花一轮才能爬出来

```
2026/09/11 00:57:21 [tool-router] id=fd9a24ae-... decision=call count=1 names=[ExitPlanMode] elapsed_ms=27984
2026/09/11 00:58:01 [tool-router] id=5fe58faa-... decision=call count=1 names=[grep]
```

路由轮又用了一次 **28 秒**的决策空间选 `ExitPlanMode`（附带整份计划）才脱离，直到 00:58:01 起才恢复常规工具（`grep`/`glob`/`read`/`pwsh`）。

---

## 2. 机制：为什么会被选中，以及为什么代价这么大

### 2.1 工具通道是单点的

整个 12.5h 窗口内 **53 次回答轮（`stage=answer_start`）的 `native_tools` 全部为 0**：

```
53 native_tools=0
```

即：客户端声明的 42/53 个工具**只存在于路由轮**。路由轮一旦判错，本轮就再也没有第二次机会去调工具。
路由轮的提示词本身就写明了偏好——"选出**能推进工作**的那一次调用"，而 `EnterPlanMode` 在语义上最像"一步到位"，
实际却是**唯一会中断执行**的选项。**这是一个结构性偏见，不是偶发幻觉。**

### 2.2 同类偏见早有反制，"模式切换类"是漏网的

仓库里已经为"委派类工具"写过反制（`internal/web/model_tool_router.go`）：

> `The router decides ONE next step, which structurally biases the model toward the delegation tool:
> "the single call that does the most work" is always the subagent launcher...`

但全仓对 `EnterPlanMode` / `ExitPlanMode` **零命中**（`grep -rn "EnterPlanMode" internal/ --include=*.go` → 0 行），
即"看起来最像干活、实际不干活"的第二类工具（模式切换）从未被处理。

### 2.3 12.5h 内路由选中的工具分布

| 工具 | 次数 | 工具 | 次数 |
|---|---|---|---|
| read | 10 | read_file | 3 |
| get_weather | 8 | get_time | 3 |
| Read | 8 | edit | 3 |
| pwsh | 6 | write_file | 2 |
| grep | 6 | glob | 2 |
| Bash | 6 | TodoWrite | 2 |
| **ExitPlanMode** | **1** | **EnterPlanMode** | **1** |

模式切换类只被选中 1 次，但**一次就是致命的一次**——它不产生错误日志，只产生一份漂亮的计划。

---

## 3. 第二个缺陷（同类症状，尚未修复）：回答轮的 CALL_TOOL 兜底从未生效

`05f8a20 feat(answer): give the answer turn its own CALL_TOOL channel` 早就给回答轮准备了工具通道，
字符串也确实在线上二进制里（`TOOL EXECUTION PROTOCOL` / `CALL_TOOL:` / `EXECUTION MANDATE` 各命中），
但**它在线上从未被触发过**：

| 指标 | 12.5h 计数 |
|---|---|
| `router-nocalls`（路由判 NO_TOOL_NEEDED → 掉进回答轮） | 23 |
| `answer_start`（回答轮总次数） | 53 |
| `[answer-tool]`（回答轮真的发出工具调用） | **0** |

样本请求 `819ced49-a22e-497c-8c74-6d7d79d2b5a9`（2026-09-10 12:48:46 UTC）：

```
messages=60 tools=53  prompt_flattened prompt_len=129252
router_start prompt_len=47948
[router-nocalls] ... upstream_text="NO_TOOL_NEEDED"
answer_start prompt_len=158251 native_tools=0      ← 129252 + 约 29KB 工具协议目录，协议确实注入了
http_return total_ms=48422  status=200 bytes=5502
```

48 秒后返回的是一段纯文本拒绝（客户端侧同一请求的响应文本）：

> 当前会话无法对 `D:\NetPeek` 执行本机文件修改，因此这次未能把改动实际写入并验证。需要在具备该 Windows 工作区操作能力的编码会话中继续…

也就是说：**协议注入了、执行授权也注明了，模型仍然选择用文字回答"我改不了文件"**。
这解释了同类症状的另一半——只要路由轮判了 `NO_TOOL_NEEDED`，兜底通道形同虚设，本轮必然以文字收场。

---

## 4. 已实施的修复

核心原则：**"进入计划模式"永远不是执行请求的下一步**，因此在执行请求下把它从候选集中摘掉；`ExitPlanMode` 保留（它是"执行请求落在计划模式里"的唯一逃生口）。

| 位置 | 改动 |
|---|---|
| `internal/web/model_tool_router.go` | 新增 `isPlanModeEntryTool` / `withoutPlanModeEntry` / `withoutPlanModeEntryTools` / `planModeEligibleTools` / `planModeEligibleToolDefs`；名字做归一化（`EnterPlanMode` / `enter_plan_mode` / `enter-plan-mode` / `planmode` 都命中）；路由提示词新增反制规则（仅在工具存在时出现，避免点名不存在的工具） |
| `internal/web/router_prompt.go` | `buildRoutePrompt` 在拼提示词前按"最新用户消息是否为执行请求"过滤候选工具 |
| `internal/web/server.go` | `buildAnswerRequest` 同步过滤：回答轮的文本协议目录与非 native 模式的 `req.Tools` |

**生效条件**：`userMessageDemandsAction(最新用户消息)` 为真（沿用既有标记表：实现/修改/修复/写入/创建/运行/继续完成…）。
用户明确要求"先出计划"时（不含执行类标记），工具集**原样保留**，计划模式照常可用。

**回退开关**：`M365_ROUTER_ALLOW_PLAN_MODE=true` 一键恢复旧行为。

### 回归测试（`internal/web/model_tool_router_test.go`，全部通过）

- `TestRouterWithholdsPlanModeEntryOnExecutionRequest` —— 用线上的原句 `实现以上未落地的内容` 复现：`EnterPlanMode` 必须消失，`ExitPlanMode` / `Read` / `Edit` / `Agent` 必须留存，且路由提示词里不再出现该名字
- `TestPlanModeEntrySurvivesNonExecutionRequest` —— 非执行请求工具集不被改动
- `TestPlanModeEntryNameNormalization` —— 命名归一化命中/误伤边界（`EnterPlanModeX`、`Plan`、`TodoWrite` 不误伤）
- `TestAnswerTurnWithholdsPlanModeEntryOnExecutionRequest` —— 回答轮协议里同样摘除，合法工具不受影响
- `TestPlanModeFilterKillSwitch` —— 开关可回退
- `TestRouterPromptWarnsPlanModeIsNotWork` —— 反制规则只在存在该工具时出现

验证结果：`go build ./...`、`go vet ./internal/web/`、`go test ./...` 全部通过。

---

## 5. 待决

1. **部署**：本次修复**未部署、未提交**。当前工作区还存在**另一个并发会话的未提交改动**（`internal/chathub/connpool.go`、`client.go`、`server.go` 的预热连接身份修复，文件修改时间在 09:17–09:23 之间，且 `m365-copilot2api-linux-amd64` 正在被其反复重建）。从工作区构建会把这些改动一起打进去，因此部署时机需要你确认——是等那份工作收尾后一起上，还是只挑本次改动单独构建。
2. **缺陷 2（回答轮 CALL_TOOL 兜底 0 命中）**：建议作为下一个独立项处理。可先做的低成本改动是给回答轮加"只允许协议输出"的强约束与一次重试，或在 `NO_TOOL_NEEDED` 且判定为执行请求时，把该轮改判为 `required` 重跑路由（提示词里的禁令已被证明不足以约束）。
3. **可观测性**：`answer-tool` 只在真的发起调用时才写日志，因此"兜底通道完全没工作"这件事在监控上是隐形的。建议补一条"回答轮未按协议输出"的计数日志。
