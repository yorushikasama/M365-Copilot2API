# 版本号显示（`/api/version` 与侧边栏徽章）

## 不变量

**徽章必须永远是一个版本号，任何构建路径下都不允许出现 `development`、伪版本串或纯 `dev`。**

版本号回答的唯一问题是"这是哪个发布版"，因此它**不能**依赖那些可能缺失的东西。历史上两次故障都出在这里：

| 日期 | 现象 | 构建条件 | 原因 |
| --- | --- | --- | --- |
| 2026-09-11 早晨 | `v0.6.7-0.20260910124631-828e899df8af+dirty` | 有 `.git`、无 `-ldflags` | Go 把主模块版本写成伪版本，回退链直接原样返回 |
| 2026-09-11 10:11 | `vdevelopment` | 无 `.git`（源码副本）且无 `-ldflags` | `mod … (devel)` 且无 `vcs.*`，回退链没有任何可用输入，返回字面量 `development`，前端又补了个 `v` |

结论：**只要版本号来自"构建时能被推断出来的东西"，就一定会再次出错。** 所以版本号必须是源码的一部分。

## 取值链（`internal/web/version.go`）

按权威性从上到下，第一个给出合法 `X.Y.Z` 的胜出：

1. `web.Version` —— 发布构建由 `-ldflags -X` 注入（tag 值，如 `0.6.7`）。
2. Go 记录的主模块版本，经 `releaseFromModuleVersion` 归一化：精确 tag 直接采用；伪版本 `v0.6.7-0.<时间>-<sha>` 取它"正在走向"的发布号 `0.6.7`；跳过 `(devel)`、`v0.0.0-*`（无可用 tag）。
3. `internal/web/VERSION` —— 由 `//go:embed` 编译进二进制。**它随源码一起走，所以任何构建路径都不会缺。**
4. `dev-<12位sha>[-dirty]` —— 仅当二进制自带 vcs 信息时（纯诊断用）。
5. 兜底 `dev`。

第 1、2 步可以同时失效（无 `.git` + 无 ldflags），第 4 步也可以失效；第 3 步不会。

## 各构建路径的结果

| 构建路径 | Version 注入 | `.git` | 显示 |
| --- | --- | --- | --- |
| CI 发布（`release.yml`） | ✅ tag | ✅ | `v0.6.7`（tag） |
| 本地 `go build`（仓库内） | ❌ | ✅ | `v0.6.7`（伪版本归一化；与 VERSION 一致） |
| 源码副本 / 沙箱 / `-buildvcs=false` | ❌ | ❌ | `v0.6.7`（VERSION 文件，本次修复目标） |
| `Dockerfile` | ❌ | 取决于构建上下文 | `v0.6.7`（同上） |
| `go install …@v0.6.8` | ❌ | — | `v0.6.8`（模块版本优先于 VERSION 文件） |

## 版本号怎么维护

`internal/web/VERSION` 是唯一需要维护的地方，且**不需要手动改**：

- 发布 `v0.6.8` 时打 tag，`release.yml` 的 `Record the release in internal/web/VERSION` 步骤把该文件写成 `0.6.8` 并提交到 `main`。
- 因此 `main` 始终等于"最新发布版"，所有非发布构建都显示它，不会出现"忘了 bump 就显示旧号"以外的偏差。

## 前端

`internal/web/web/index.html`（唯一一份，由 `//go:embed` 编进二进制，`frontend_version_test.go` 守着）：

```js
if(v)el.textContent=/^\d/.test(v)?'v'+v:v;
```

只有**以数字开头**的值才补 `v`。后端哪怕回归成返回 `development`，徽章也只会显示 `development`，不会再被拼成 `vdevelopment`。

## 回归测试（`internal/web/version_test.go`）

- `TestReleaseFromModuleVersion` —— 用线上真实串 `v0.6.7-0.20260910124631-828e899df8af+dirty` 等 13 个输入断言归一化结果。
- `TestSourceVersionIsARelease` —— `VERSION` 文件必须是纯 `X.Y.Z`。
- `TestEffectiveVersionAlwaysAnswersWithAVersion` —— `Version=dev` 时结果必须读起来像版本号，且不得为 `development`（本次故障的直接回归）。
- `TestEffectiveVersionNeverLeaksProvenance` —— 不得含 `+dirty` / `-0.20…`。
- `TestEffectiveVersionPrefersInjectedVersion` / `TestEffectiveCommit` —— 注入值优先、commit 永不返回空。

## 注意

- `internal/web/VERSION` 是**新增文件**，提交时不能漏：`//go:embed VERSION` 在文件缺失时直接编译失败。
- 不要把版本号再挪回"只由构建时推断"的路子（例如删掉 VERSION 文件、只留 `debug.ReadBuildInfo()`）。
