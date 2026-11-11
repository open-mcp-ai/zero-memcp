# 审计：termcp 已公开什么，我们重写了什么

写这份是因为「这个该不该找 termcp 要」反复被问，而没有事实清单就只能猜。
每一条都标明**证据**（文件:行）与**判定**。

判定只分三种：

- **接上就行**：termcp 已公开，是我们没接
- **该提 issue**：机制在 termcp 里跑着，但只在 internal 下
- **不该提**：termcp 没有，提了等于要新功能

---

## 一、已经在用的（不是重复）

| 能力 | 来源 |
|---|---|
| 语法解析（`;` `&&` `\|\|` 后台组） | `term.NewGrammar` / `term.New` |
| payload 变量强制 | `Op.PayloadType` |
| 信封渲染 | `envelope.Reply.Text()` / `.Structured()`（`term_verbs.go` 的 `renderReply` 只是投影） |
| 地址解析 | `term/locator` |

**注意**：早先我曾判断「`renderReply` 自己拼了信封」——**错了**。它调的是
`envelope.Reply` 的方法，信封格式由 termcp 保证。这一条已更正。

---

## 二、已修：帮助系统

**问题**：`Op.Detail` 是公开字段，但读它的渲染逻辑在
`termcp/internal/mcp/term_generated.go:1474-1488`，嵌入者拿不到。于是我们写了
544 行 `help.go`，而 46 个 `Op` 的 `Detail` 一个都没填。

**处理**（`f53abb3`）：数据搬进 `Op.Detail`（`internal/server/help_details.go`），
`help.go` 544 → 97 行，剩下的只是一个循环。渲染仍由我们这层薄壳做，等 termcp
导出渲染器后整段删掉——**数据只有一份，不返工**。

---

## 三、待办：MCP 协议层（约 380 行）

我们自己实现了：

```
server.go:1036  handleMCP         ~70 行   认证、SSE、请求体
server.go:1105  dispatchMCP      ~170 行  initialize / tools/list / tools/call / subscriptions/listen
server.go:1277  callTool         ~114 行  参数解码、投影
server.go:1391  buildToolResult   ~25 行  text / structured 两种投影
```

而 termcp 的 `mcp` 包提供了同一层：

```go
mcp.NewServer(name, version, instructions, Hooks)
srv.AddTool(mcp.Tool, mcp.ToolHandler)
srv.OnMethod("subscriptions/listen", h)          // 扩展点
srv.Handle(ctx, raw []byte, sess Session) []byte // 传输无关
srv.NotifyAll(ctx, method, params) int           // 服务端推送
type Session interface { SessionID; Notify; GetClientInfo }
```

**判定：接上就行**（全部公开）。

**风险**：`tools/list` 的输出形状由 `mcp.Tool` 决定，我们的 schema 是一张手写
`map[string]any`（`server.go:1194` 起）。迁移会改变客户端看到的 schema 字段
顺序，可能影响已有断言与 agent 的提示词。

**建议做法**：先写一个 parity 测试——同一份 schema 分别用两条路径序列化，
逐字段比对；一致才切默认路径。

---

## 四、待办：事件订阅（约 200 行）

```
internal/server/resource_events.go   订阅注册、SSE 帧
internal/server/tree_events.go       树变更 → 事件 → 按订阅过滤
```

termcp 侧：`Session.Notify` / `Server.NotifyAll` **已公开**（`mcp/server.go:59,465`），
`WithSessionContext` / `SessionFromContext` 也在。

**判定：接上就行**。缺的只是「谁订阅了哪个 uri」这张表——那是业务，我们本来就该写。

---

## 五、不该提的

| 项 | 原因 |
|---|---|
| 单实例锁 | termcp 没有（搜 `flock`/`LockFileEx` 零命中），提了是**新功能** |
| CLI 骨架 | 同上，且和它的子命令结构绑死 |
| daemon 生命周期 | 是 termcp 守护自己的逻辑，抽出来对面要改架构 |
| 扩展托管 | 和它的安全模型（`allow` 白名单）绑太紧 |

---

## 六、唯一该提的 issue

> **expose the help renderer so `Op.Detail` reaches embedders**

`Op.Detail` 公开、渲染私有。要求导出一个 `help <verb>` 的渲染入口，判据是
「只声明了 `Op{Usage, Detail, Flags}` 的外部程序，`help <verb>` 与
`help <verb> --all` 的输出与 termcp 自身同构」。

这是唯一一条「代码已经在跑，只是没导出」的缺口。
