# M365-Copilot2API 项目长期笔记

## 服务器与部署
- 服务器 `161.33.164.136`（ubuntu，key = 桌面 `ssh-key-2026-08-15.key`），服务 `m365-copilot2api.service`，二进制 `/opt/m365-copilot2api/m365-copilot2api`，数据目录 `/var/lib/m365-copilot2api`，监听 `127.0.0.1:4141`。
- **服务器时区 = UTC**，日志时间 +8 才是北京时间。
- 部署流程：本地 `go build`（linux/amd64）→ 备份 `/opt/.../m365-copilot2api.bak-pre-<commit>` → scp → 重启服务 → 验证（HTTP 200 + 关键日志串 + 二进制 md5）。历史备份文件很多，按名字对得上 commit。
- 仓库里带着 `m365-copilot2api-linux-amd64` 二进制，每次提交都会带上；构建产物变更属正常。
- **推送 GitHub 受本机代理限制**：直推常被静默吞掉（exit 0 无输出、远端 ref 不动）。可行路径是借道服务器中转（bundle scp 上去 → 服务器 clone --bare + fetch bundle + push）。验证远端状态一律用 `git ls-remote`，别信本地 `origin/main`（本机 ref 读取有陈旧值的老毛病）。

## 排查网关问题的方法（关键）
- 网关日志（`journalctl`）只有决策摘要：`[req-trace] stage=...`、`[tool-router] decision=... names=[...]`、`[router-nocalls] upstream_text=...`、`[answer-tool]`、`[router-slim]`。**看不到请求正文与模型推理**。
- **取全文的唯一途径**：把网关日志的 request id 与客户端会话存档对上 —— `~/.zcode/cli/rollout/model-io-sess_*.jsonl` 里每条记录含 `request.messages`（完整历史）与 `response`（`finishReason`/`text`/`reasoningText`/`toolCalls`/`usage`），且 `response.headers.x-request-id` **就是网关的 request id**。这是定案级证据。
- 服务器 `conversations.json` 只存云会话映射，且会被 auto-cleanup 清空，不是取正文的地方。
- 排查"网关侧没反应/没干活"先查服务器 journalctl 全链路，别先翻本地工具日志。

## 架构要点（影响排障判断）
- 工具通道**单点依赖路由轮**：`answer_start` 的 `native_tools` 恒为 0，工具只经路由轮下发（`[tools] declared=N transport=api-plugins`）。**路由轮判错 = 本轮没有任何工具可调**。
- 回答轮有独立的文本 `CALL_TOOL` 协议兜底（`answerToolProtocol`），但 2026-09-11 实测 12.5h 内 `[answer-tool]` **0 命中**（23 次 nocall 机会全落空）——兜底不可依赖。
- **回答轮协议闸门（2026-09-11 修，10da981）**：`internal/web/answer_gate.go` 的 `answerProtocolGate` 负责把 `CALL_TOOL:` / `{"calls"` 挡在正文通道外。核心坑：判定函数必须**前缀安全**——上游首包只有 4 字节（`CALL`），旧 `classifyAnswerOutputPrefix` 会在首包就判定"不是工具调用"并放行，导致客户端收到字面协议行（`decision=deferred_call` 日志 0 次 = 该分支从未运行）。改协议闸门时不要退回"看到开头就下结论"。
- **`server.go` 的 `openaiChat` 里有两条完整且重复的流式回答实现**（约 2257-2543 与 2663-2939）。第二条对 `stream=true` 不可达（第一条全路径 return）= 死代码；改流式回答行为**两处都要改**或先确认哪条在跑（用日志格式区分：前者打 `text_emitted=`）。
- `stage=http_start stream=%t` 读的是 **URL query**，真实分支看 `body.Stream`——ZCode 在 body 里传 stream，日志里的 `stream=false` 是假象，排查时不要被带偏。
- 路由提示词会 `[router-slim]` 瘦身（`tail=4 max_bytes=4096`），带引用指代（"以上"）时走 `upgrade_to_bounded`。
- 大量行为开关走 env，便于回退：`M365_ROUTER_FULL_TOOL_SCHEMAS`、`M365_ROUTER_FULL_ON_REFERENCE`、`M365_ENABLE_INCREMENTAL_PROMPT`、`M365_ENABLE_UPSTREAM_MEMORY`、`M365_ROUTER_ALLOW_PLAN_MODE`（新增）。**改动路由/提示行为时一并给开关**是本仓惯例。

## 协作注意
- 本仓可能被**多个 agent 会话并发编辑**（2026-09-11 09:17–09:23 实测）。动手前先 `git status`；**不要 stash / 回滚 / 覆盖**不属于本次任务的未提交改动，构建部署前要确认工作区里没有别人的在途工作。
- 用户偏好：结论先行（根因 + 修复 + 风险 + 证据链）、结构化表格、一次给全；**反感只列计划不执行**。
