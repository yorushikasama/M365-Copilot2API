# M365-Copilot2API 安全审计报告

> 审计范围：`internal/web`、`internal/auth`、`internal/chathub`、`internal/outbound`、`internal/mcp`、`cmd/server`、`docker-compose.yml`、`Dockerfile`、前端 `internal/web/web/index.html` 及落盘数据文件。只读分析，未修改任何代码。
> 部署背景：默认监听 `127.0.0.1:4141`（cmd/server/main.go:23-26）；docker-compose 仅映射回环端口。以下风险在「暴露公网 / 多租户分发 API key」场景下放大概率最高。

---

## 一、严重

### S-1 默认管理员口令 admin123，未配置即一击接管

- **风险点**：`internal/web/admin_security.go:15` 定义常量 `const defaultAdminPassword = "admin123"`；`loadAdminPassword`（:35-51）在未配置 `M365_ADMIN_PASSWORD` / bootstrap 文件 / 环境变量时静默回退 `admin123`（返回 `mustChange=true`）。密码本身以明文写入文件（:52-58）。登录获取会话后调用 `POST /api/admin/change-password`（admin_security.go:132-158，在 adminMiddleware 白名单 server.go:157）即可改任意新密码完成接管。
- **可利用场景**：管理员未设置密码环境变量即上线（如直接 `go run`、容器未挂载 secrets/）；攻击者用 `admin123` 登录 → 强制改密 → 长期会话（server.go:245，7 天）→ 访问全部管理端点：导出所有明文 API key、翻阅 M365 账户令牌、配置恶意代理池与部署端点。
- **修复建议**：无密码配置时拒绝启动并打印明确指引；移除默认口令或启动时生成随机口令并强制交互式修改；密码用 salted hash（argon2/bcrypt）存储而非明文；登录错误不区分「用户不存在/密码错误」。

### S-2 图像附件「任意 URL 下载」构成 SSRF

- **风险点**：`internal/chathub/client.go:421-447` 对附件 URL 执行 `http.NewRequestWithContext(GET, a.URL)` 后先 base64 再上传微软；URL 来源为 `/v1/chat/completions`、`/v1/messages`、`/v1/responses` 消息内容中的任意 `image_url` / `attachment`（`internal/web/multimodal.go:28-60`、`chathub/images.go:43-53`）。仅限制 scheme 为 http/https（`https:`/`http:` 校验，`data:` 走 base64 直传），无 private/loopback/metadata 地址过滤，无 DNS rebinding 防护。
- **可利用场景**：持任意有效 key 的调用方把网关当内网跳板——探测本机/内网端口与云元数据（`http://169.254.169.254/latest/meta-data/`）、对局域网设备/管理面板发起定向 GET、并借助「响应体 base64 打包上传至绑定的 M365 会话」实现带出数据；还可对第三方站点伪造来源发动请求。
- **修复建议**：下载前解析并校验目标 IP（拒绝 private/loopback/link-local/metadata 段、DNS 解析后重校验防 rebinding）；仅允许 `https` 且宿主机不位于内网；或干脆仅接受 `data:` URL、禁用远程直传（默认不开）。

### S-3 跨请求状态无 API key 租户隔离 → 多租户数据串读

- **风险点**：三个跨请求共享态均未绑定 API key 命名空间：
  - `/v1/responses` 的 `previous_response_id` 池：进程级 `responseMessages` 全局 map（`internal/web/protocol_handlers.go:242-250`、:312-314），任何持有他人 `resp_` ID 的调用方可读取并续接该会话历史。
  - 客户端可自由传 `X-M365-Session-Id`：`internal/web/session_resolver.go:225-257` 命中即复用他人会话；内容前缀匹配（:327-349）进一步放大「同内容即串话」；`/v1/chat/completions` 的普通会话 `user` 字段写入 userSessions（internal/server.go:833-845，sessions.go:136-166）。
- **可利用场景**：自建网关向多人/多企业分发 key 时，A 调用方可读 B 的历史对话（提示词、附料、业务敏感信息），攻击者利用高命中率的 session_id 直接接管占用他人会话上下文、或误导下游。
- **修复建议**：所有跨请求状态（responseMessages、sessionResolver、userSessions、conversationManager）一律以「API key/所属租户」作前缀隔离；`previous_response_id`、`X-M365-Session-Id` 校验必须同时绑定创建方同一 key；修「优先状态下命中同 key」逻辑。

---

## 二、高

### H-1 API key 明文落盘 + 管理接口明文返回

- **风险点**：`internal/web/keys.go:60-61` `create()` 将 `r.Raw = raw` 写入记录并 `save()` 持久化为 `data/api-keys.json`（keys.go:44-53，0600）。审计实测该文件已知含明文 key（`"raw": "m365_...`）。`list()`（keys.go:71-80）仅清 `Hash` 字段、不清 `Raw`，`GET /api/admin/keys`（internal/server.go:268-271）直接前端以明文返回全部 key（含已吊销）。
- **可利用场景**：任何能读进程工作目录文件（备份、镜像层、token 泄露、容器读取卷）的实体即获得所有租户共用 API key；管理面板凭 admin 会话明文查看全部 key。
- **修复建议**：`create` 完成校验后立即清空 `Raw` 不再序列化；`list`/已吊销记录返回 `m365-....<last4>` 格式；密钥文件仅 0600 且明确加入 .gitignore；如需导出，改为单次一次性响应。

### H-2 M365 账户 `access_token`/`refresh_token` 明文落盘

- **风险点**：`internal/auth/cache.go:12-24` `AccountToken` 结构含 `AccessToken`、`RefreshToken`、`Scope` 等，`saveLocked`（:97-101）以 0600 全量写 `data/accounts.json`。实测文件中含**整段明文 refresh_token（长期）**与 access_token、账号邮箱、displayName。
- **可利用场景**：refresh_token 利于 60-day 长期会话在泄露时自持用（如公钥泄露、卷被读）；若容器环境变量/挂载被攻破，直接拿走 M365 用户权。
- **修复建议**：`accounts.json` 内的 token 以 OS 密钥库/AES-GCM 加密（密钥由 `M365_TOKEN_ENC_KEY` 环境变量提供）；或至少文档明确警告并建议短期 access/轮换 refresh；存储目录不进镜像/卷只读策略。

### H-3 `local/` MCP server 与上传 SSRF 放大器待确认配置（上面 S-2 已覆盖）→ 见上

- 已并入 S-2，此处不重复。

### H-4 匿名可用的 `/api/stats` 与 `/api/stats/reset`

- **风险点**：`internal/web/server.go` 将 `/api/stats`（:142）与 `/api/stats/reset`（:143）注册调用 `handleCacheStats`/`handleCacheStatsReset`；这两个路径已加入 adminMiddleware 白名单（server.go:157），其中 `handleCacheStats`（conversations.go:97-116）无自身鉴权。`cache_stats.go:20-34` 输出每个 API key 的请求/响应 token、命中次数与活跃会话数，只含 key 前缀。
- **可利用场景**：任何知道网关地址者可匿名读取服务用量、推断 key 活跃度/规模；可无限调用 `reset` 清空管控统计（干扰管理员监控，DoS 管理视图）。
- **修复建议**：`/api/stats` 与 `/api/stats/reset` 移出白名单，纳入 admin 会话校验；统计字段确认不暴露完整 key。

### H-5 无速率/用量限制，`valid()` 每次校验即全盘写

- **风险点**：调用 key 校验须在 `internal/web/keys.go:118-126` `valid()` 中更新 `LastUsedAt` 后**每次都对整个 json 做 `s.save()`（:125）**；随后 `bindConversation` 还会立即再写 `sessions.json` 与 `conversations.json`（internal/server.go:1318-1320）。port 端  亦无 per-key RPS/令牌桶、无按 key 并发上限（仅有 ChatTimeoutSeconds 超时与 10MB body）。
- **可利用场景**：持有任意 key 者高并发刷 `/v1/*` 接口 → 重磁盘 I/O DoS（IO 放放大）、打爆上游 M365 Copilot 账号对话配额导致封禁；多 key 相互拖慢。
- **修复建议**：`LastUsedAt` 改为仅（内存）更新，异步批量落盘；`/v1/*` 按 key 加 RPS/突发限制（如 token-bucket）。

---

## 三、中

### M-1 明文对话/查询数据落盘（debug、session、conversation）

- **风险点与位置**：
  1. `internal/web/debug.go:95-99` 写 `debug-logs.jsonl`（默认当前工作目录，Docker 内 /app 非数据卷，容器重启丢失；本机运行则落盘在启动目录）。`.debug` 中间并`debugHandler`（:184-187）记录**每条 `/v1/*` 请求的完整原始 body（含用户对话、data URL base64 图、工具输出）与完整响应**；`redact` 只脱敏顶层 `api_key/access_token`（:46-58），嵌套的 attachments/messages 仍明文。
  2. `internal/web/session_resolver.go:102`、字段 33 —— `sessions.json` 存明文 `ContextHistory`（完整用户消息）。
  3. `internal/web/conversation_manager.go:103-116` —— `conversations.json` 的 `Title` 记录首轮完整 prompt（实测含整段 system 指令）。
- **可利用场景**：任何能读到这些文件（备份、Docker 卷宿主机访问、容器代码路径）即获取敏感业务文本与浏览量。
- **修复建议**：debug 日志默认关闭或不落盘；`redact` 递归脱敏；conversation `Title` 只记摘要/首 50 字符；文件放专用 0600 目录并在文档标注「明文」警告。

### M-2 前端引用不可信可变脚本，CSP 放宽

- **风险点**：`internal/web/web/index.html` `<script src="https://unpkg.com/lucide@latest">`（`@latest` 每次换版本、无 integrity）；`internal/web/security_http.go:12-17` 的 CSP 允许 `script-src 'self' 'unsafe-inline' https://unpkg.com https://cdn.jsdelivr.net`。
- **可利用场景**：CDN 被投毒/供应链替换内容时，可在管理页同源下执行任意 JS，窃取全部管理数据（所有 key、凭据、代理）。
- **修复建议**：锁定 lucide 具体版本并加 `integrity`；CSP 收紧为 `'self'`（去 inline 与外域脚本）；上传改为自托管图标。

### M-3 提示注入放大（与租户隔离叠加才成立）

- **风险点**：`internal/chathub/images.go:59-66` 用户 prompt 直接拼入 `Generate an image...` 描述；`internal/web/protocol_compat.go:27-31` 把客户端 `instructions` 作为 system 指令；`internal/chathub/tool_protocol.go:34-42` 把用户文本包进 `<tools>` 指令。
- **可利用场景**：单一方注入自身意义不大；一旦叠加 S-3 的会话无隔离，A 注入的「system 指令」会被 B 复用 → 变成对 B 的主动 prompt injection（跨身份提示注入放大器）。
- **修复建议**：以 S-3 会话命名空间隔离为前提；对上游 `instructions`/description 若含工具相关标签可做剥离/限制长度；在传给 Copilot 的 system 前加明确单引号括起限制。

### M-3 错误信息回显上游明细

- **位置**：`internal/web/m365cloud.go:148,153` 将 MS API 响应 body 前几百字节直接拼进错误 JSON 逐字返回给管理端（`handleM365Delete/ListConversations`）；`internal/auth/token.go:84-90` 与 `device.go:114` 将 token endpoint 原始 body 一并回显。
- **可利用场景**：管理接口在 M365 报错时可能回显会话体元数据/OCP 头部细节，辅助内格信息收集。
- **修复建议**：对外错误消息统一收敛为常量模板（过滤上游 body），仅打日志详细。

---

## 四、低

1. **WebSocket `access_token` 放进 URL query** — `internal/chathub/buildClient.go / client.go:399-419`：URL 会出现在代理 CONNECT、反向代理 access.log、链路 trace 中。建议改用 `Authorization` 头，或在部署文档警告代理日志不可泄。
2. **HTTPS 代理在 host 为 IP 时跳过 TLS 证书校验** — `internal/outbound/proxy.go:200-201`（`hpp.FirmName` 逻辑），配合 `M365_PROXY_INSECURE_TLS` 仅可在自管代理关闭，需文档标注不可用于生产代理。
3. **`/api/chat`、`/api/chat/stream` 无 body 大小限制** — `server.go:916`、`stream.go:20` 用 `json.NewDecoder` 无 MaxBytesReader；`debugMiddleware` 仅对 `/v1/` 生效。管理接口可被长请求耗尽内存。
4. **`responseMessages` 与 PKCE `state` 池无过期清理** — `protocol_handlers.go:312-314`、`server.go` 中 pkce state map：长期运行内存缓慢增长（低危 DoS）。
5. **`clientIP` 在 RemoteAddr 为 loopback 时信任 `X-Forwarded-For`** — `internal/web/admin_security.go:71-81`：若反向代理不覆盖/剥离 XFF，攻击者可伪造成任意 IP（如管理员 IP）进入登录失败锁定 / 铁围 401 风暴。建议文档要求反向代理覆盖 XFF。
6. **GET 未认证路径统一返回 401 — 存在枚举面** — middleware 先于 404（server.go:157 白名单外的路径）。轻微探测标记。

---

## 五、已排查确认「不算漏洞」（有意排除，避免误报）

1. **SSE/流式协议注入（XSS via event）** — 所有 `data:` 帧均经 `mustJSON`/`json.Marshal`（`server.go:968,1135,1344`；`internal/web/tool_response.go:102`；`protocol_response.go:49-68`；`stream.go:96-98`），`\n`、CR、`<`、`>`、`&` 均被 JSON 转义，`writeSSE` 的 `event:` 名称为常量。无 CRLF/`<script>`/字段逃逸。
- **CORS/CSRF**：主站（`internal/web/web/index.html` fetch → `/api/*`）未设置任何 `Access-Control-Allow-Origin`，浏览器跨域被同源阻止；admin cookie `SameSite=Lax`（server.go:247）。`internal/mcp/server.go:55,118,156` 的 `Access-Control-Allow-Origin: *` 为**死代码**——`/v1/mcp/tools|sse|message` 从未在任何 `Routes()` 挂载（全项目 grep `HandleToolsList/HandleSSE/HandleMessage` 仅出现在定义处，main.go 也从未调用 `mcp` 包），不构成暴露面。
- **命令注入**：全库无 `os/exec`（`internal/mcp/client_test.go:47` 仅测试引用）；bash 工具块（`internal/web/fenced_tools.go:9-24`）只把工具调用编码为 `tool_calls` **返回客户端执行**，网关从不执行。
- **路径穿越/目录遍历**：`security_http.go` 从嵌入式 `internal/web/web/index.html` 提供根页面（static，不展开任意路径），非用户可控路径；写盘路径全部来自环境变量（管理员控制）。
- **API key 认证绕过**：仅接受 `X-API-Key` / `Authorization: Bearer`，不支持 query 参数或变体；无需内置默认 key；中间件用 `url.Path` 前缀匹配（`/v1/`），`/v1/../api/accounts` 等会被 ServeMux 先 `../` 清理并 301，无法进入 `/api/` admin 分支绕过鉴权。
- **登录暴力破解**：已实现5次失败锁15分钟（`internal/web/admin_security.go:104-126`）并受 4096 上限保护，属合理防护（仅提示反向代理需篡改XFF见上）。

---

## 修复状态（2026-08-07 第二批）

- **[已修复] S-2（SSRF）**：`internal/chathub/ssrf.go` `validateRemoteDownloadURL` 在附件/图片下载前解析目标，拒绝 loopback/private/link-local/multicast/CGNAT/169.254.169.254 元数据段，且仅允许 https。
- **[已修复] S-3（响应池租户隔离）**：`s.responseMessages` 改为按 `X-API-Key`/`Bearer` 命名空间嵌套 map，`previous_response_id` 严格限定本租户；每租户 256 上限 + 1 小时过期淘汰。
- **[已修复] H-1（API key 明文）**：新建 key 不再落盘 `raw`（仅存 hash+prefix）；`list()`/create 返回均抹去 hash/raw；读取时自动迁移历史明文记录并回写。
- **[已修复] 低项 3（body 限制）**：`/api/chat`（`server.go`）与 `/api/chat/stream`（`stream.go`）接入 `http.MaxBytesReader` 10 MiB。
- **[已修复] M-1.1（debug 脱敏深度）**：`redactBody` 递归脱敏敏感键（api_key/access_token/secret/password 等 16 类），覆盖嵌套请求/响应。
- **[已修复] M-3（错误回显收敛）**：`m365cloud.go` 不再把上游 body 逐字拼进错误；`token.go`/`device.go` 错误仅保留状态码与 error 字段，不再回显原始响应体。
- **[已修复] 低项（admin 会话无界 map）**：`adminSessions` 加 4096 上限 + 最旧淘汰泛化处理；`loginAttempts` 已有 4096 上限与 15 分钟过期清理。
- **[未做，风险已知] H-2（M365 token 明文落盘）**：`access_token`/`refresh_token` 仍明文存于 `auth` store；加密迁移工程量较大，当前以 0600 权限缓解，建议后续 AES-GCM 改造。
- **[跳过，不符合现状] S-1 省略**：默认口令提示已由多个入口覆盖，按用户决策不启用强制校验。

## 修复优先级建议（原始评估）

1. 立刻：设置 `M365_ADMIN_PASSWORD`（勿用默认值不空跑）；`/api/stats`、`/api/stats/reset` 加鉴权。
2. 高：修复 S-2（禁远程 image 直传或加内网白名单）、S-3（会话/responses 池按 key 隔离）、H-1/H-2（明文密钥改加密落盘）。
3. 中：debug 日志脱敏、CDN 锁版本收紧 CSP、token/错误回显收敛。
4. 低：速率限制、key 存内存 update、反向代理 XFF 规范、PKCE/响应池清理。

---

*行号均为实际代码中已核对行（项目根目录为 D:\M365-Copilot2API）。*