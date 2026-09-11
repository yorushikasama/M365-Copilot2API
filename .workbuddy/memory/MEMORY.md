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
- **`server.go` 的 `openaiChat` 里有两条完整且重复的流式回答实现**（A: 2257-2591，B: 2664-2939）。**用花括号深度脚本已确认 A 块的 `if body.Stream {` 在 2591 关闭且全路径 return**，故 B 对 `stream=true` 不可达 = 死代码；**改流式回答行为只改 A**（除非将来修可达性）。区分哪条在跑看日志：A 打 `text_emitted=`。
- **流式 rewrite 会丢内容（2026-09-11 修）**：上游会在流式中途**重写**已发内容（24h 20 次 / 1134 请求，其中 10 次真实丢内容，最高丢 84%）。`emitSnapshot` 见非前缀快照即抑制后续 delta；`finalizeText` 的 diverged 分支原本**只把 final 写进 `Result.Text`、不补发** → 流式客户端永远停在废弃片段上（`2552b57f`：客户端 186 字节 / final 787 字节）。现按 `commonPrefixLen` 补发 `final[lcp:]`；开关 `M365_STREAM_DIVERGENCE_REEMIT=0`。
- **承诺式计划播报守卫（2026-09-11 新增）**：模型用"我会把…一起补齐"回应执行请求时 `toolCalls=[]`，现有三道守卫**全是否定式**（没有工具/无法编辑/沙箱）**全部漏过**；且 `[tool-eject]` 重试**只在非流式路径**有，流式路径只有日志。现由 `answerProtocolGate.ArmPromissory` 全程 hold（什么都不发，客户端零污染）+ 流末协议纠正重试；开关 `M365_ANSWER_PROMISSORY_GUARD=0`。
- **前缀安全 + `strings.ToLower` 是第二个坑（同一天第二次踩）**：`strings.ToLower` 会把流式半截 UTF-8 替换成 U+FFFD，**改变字节前缀**（`"我\xe4"` 不再匹配 `"我会"` 的前缀）→ 首包即误判放行。任何流式首包判定函数在用 `ToLower`/正则/`unicode` 前**必须过 `utf8.ValidString`**；再加固定字节未决窗口兜底。
- `stage=http_start stream=%t` 读的是 **URL query**，真实分支看 `body.Stream`——ZCode 在 body 里传 stream，日志里的 `stream=false` 是假象，排查时不要被带偏。
- 路由提示词会 `[router-slim]` 瘦身（`tail=4 max_bytes=4096`），带引用指代（"以上"）时走 `upgrade_to_bounded`。
- 大量行为开关走 env，便于回退：`M365_ROUTER_FULL_TOOL_SCHEMAS`、`M365_ROUTER_FULL_ON_REFERENCE`、`M365_ENABLE_INCREMENTAL_PROMPT`、`M365_ENABLE_UPSTREAM_MEMORY`、`M365_ROUTER_ALLOW_PLAN_MODE`、`M365_ANSWER_PROMISSORY_GUARD`、`M365_STREAM_DIVERGENCE_REEMIT`。**改动路由/提示/流式行为时一并给开关**是本仓惯例。

## 版本号（`/api/version` + 侧边栏徽章）
- **不变量**：徽章永远是一个版本号，任何构建路径下都不得出现 `development` / 伪版本串。
- 取值链（`internal/web/version.go`）：`-ldflags` 注入的 `Version` → 归一化后的 Go 模块版本（伪版本取"正在走向的发布号"）→ **内嵌的 `internal/web/VERSION`（`//go:embed`，随源码走，永不缺失）** → `dev-<sha>` → `dev`。
- **不要再把版本号交回"构建时推断"**：有 `.git` 无 ldflags → 显示 `v0.6.7-0.2026…+dirty`；无 `.git`（源码副本/沙箱/`-buildvcs=false`）→ `mod …(devel)` + 无 `vcs.*` → 旧代码显示 `vdevelopment`。两次真实故障都源于此。
- `internal/web/VERSION` 是唯一需维护处，且由 `release.yml` 在发布后自动写成 tag 值；**新增文件，提交时不能漏**（embed 依赖它，缺则编译失败）。
- 前端（`web/index.html` 与 `internal/web/web/index.html` 逐字节一致）：只有 `^\d` 开头的值才补 `v`。
- 详见 `docs/version-display.md`。

## 判断线上二进制"怎么构建的"（服务器无 go / 无 strings）
- 把 `/opt/m365-copilot2api/m365-copilot2api` scp 回本地，跑 `go version -m <bin>`：`mod …(devel)` + 无 `vcs.*` = 无 `.git` 副本构建；`vX.Y.Z-0.<时间>-<sha>+dirty` = 有 `.git` 但没传 ldflags；有 `vcs.revision` 才能标 commit。
- 复现"无 git 构建"：`tar --exclude=.git` 导出到**临时目录的子目录**（Go 拒绝系统临时根目录里的 go.mod）。
- 活体验证不碰线上：另起端口 + 独立 `M365_DATA_DIR` + 自写 `M365_ADMIN_PASSWORD_BOOTSTRAP_FILE` 启动，`POST /api/admin/login` 拿 cookie 再 `GET /api/version`。**除 `/v1/*` 外所有路径都要求管理员会话**（全局守卫在 `server.go:432` 附近），所以 curl `/api/health` 也会 401。
- 坑：Bash 工具调用间 `GOOS` 会串，构建一律显式写全 `GOOS/GOARCH/CGO_ENABLED`，否则 `-o x.exe` 会产出 ELF（`Exec format error`）。

## 协作注意
- 本仓可能被**多个 agent 会话并发编辑**（2026-09-11 09:17–09:23 实测）。动手前先 `git status`；**不要 stash / 回滚 / 覆盖**不属于本次任务的未提交改动，构建部署前要确认工作区里没有别人的在途工作。
- 用户偏好：结论先行（根因 + 修复 + 风险 + 证据链）、结构化表格、一次给全；**反感只列计划不执行**。
