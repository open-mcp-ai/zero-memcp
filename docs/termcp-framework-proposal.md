# 提案：termcp 从「一个终端服务」变成「一套宿主」

现状：`core` 已经把 main() 的装配抽出来了（`core.New(core.Options{Config, Version, DaemonChild, IdleTimeout})`），
但装配的是 **termcp 自己的**会话、shell、REST、UI。要变成框架，缺的不是抽象层，
而是两个注入点：**能力** 与 **认证**。

目标形状：`main.go` 三十行——解析配置、塞进能力、起宿主。

---

## 一、分层

```
┌ 能力层（嵌入者写） ─────────────────────────────┐
│  Ops()            动词表（Usage/Detail/Flags/PayloadType）│
│  Handle(statement) 每个动词做什么                          │
│  Tools()          MCP 工具声明                            │
│  Capabilities()   initialize 里声明什么                    │
│  Routes()         额外 HTTP 路由（管理面板）                │
│  Authorize()      这个主体能不能做这件事                    │
└──────────────────────────────────────────────┘
┌ 宿主层（termcp 提供） ───────────────────────────┐
│  daemon 生命周期、stdio / HTTP / SSE 传输、会话、       │
│  CLI 骨架、单实例、端口策略、配置解析、日志              │
└──────────────────────────────────────────────┘
┌ 协议层（已有，不动） ────────────────────────────┐
│  term（语法/执行）、directive（声明/解析）、envelope（信封）、│
│  mcp（协议）、locator                                │
└──────────────────────────────────────────────┘
```

## 二、能力接口

```go
package core

type Capability interface {
    Name() string
    Ops() []term.Op
    Handle(ctx context.Context, st *directive.Statement) envelope.Reply
    Tools() []mcp.Tool
    Capabilities() map[string]any          // 合并进 initialize
    Routes() []Route                       // 额外挂载
    Authorize(ctx context.Context, id *auth.Identity, a auth.Action) error
    Close(ctx context.Context) error
}

type Options struct {
    // 现有字段保持不变……
    Capability Capability
    Auth       auth.Authenticator
    ProtocolVersion string               // 空 = 默认
}
```

`termcp` 自己变成第一个实现：把 `terminal.Capability{}` 塞进去，行为和现在完全一致。
**先自己吃自己的狗粮，这个切面才算被验证过。**

## 三、通用认证模型

现在 `internal/auth` 的形状已经对了——`Verifier` + `Middleware`，认 Bearer/Basic/Cookie——
问题是它在 `internal` 下，而且是单口令。导出并抽象成三段：

```go
package auth

// 1. 主体：框架只认这个，不认 JWT / 口令 / Cookie
type Identity struct {
    ID   string            // 稳定标识
    Kind string            // human | agent | service
    Meta map[string]string // 凭证名、来源、角色……
}

// 2. 认证：请求 → 主体。返回 (nil, nil) 表示匿名。
type Authenticator interface {
    Authenticate(ctx context.Context, r *http.Request) (*Identity, error)
}

// 3. 授权：交给能力层——只有它知道「读这棵树」是什么意思
type Action struct {
    Verb   string
    Target string
    Meta   map[string]string
}
type Authorizer interface {
    Authorize(ctx context.Context, id *Identity, a Action) error
}
```

配套约定：

| 约定 | 理由 |
|---|---|
| 多个 `Authenticator` 串联，先到先得 | 同一台机器上 Bearer 与 Cookie 并存是常态 |
| 认证失败 = 401，授权失败 = 403 | 两个错误必须能分开，否则调用方不知道该换票还是换人 |
| 身份进 context：`auth.IdentityFrom(ctx)` | 能力层不接收框架类型，只取自己认识的东西 |
| 凭证的签发/吊销/列举**不属框架** | 那是能力的业务；框架只提供 `Routes()` 让能力挂自己的端点 |
| 未认证路由要能声明 | 登录、健康检查、Webhook 必须能绕过 |

## 四、四个必须留的洞（不改就永远接不上）

1. `ServerCapabilities` 可扩展（或 initialize 结果整体可由能力层给）——否则嵌入者声明不了自己的 MCP 能力
2. `ProtocolVersion` 可由 Options 指定——否则永远停在默认版本
3. stdio 传输导出（代码已在跑，只是没导出）
4. `help <verb>` 渲染器导出（同上）

## 五、zero-memcp 的迁移路径

| 阶段 | 动作 | 删除 | 前置 |
|---|---|---|---|
| 0（已完成） | 帮助数据搬进 `Op.Detail`；`tools/list` 用 `mcp.Tool` | 447 + 60 行 | — |
| 1 | 传输层换 `NewStreamableHTTPServer` + `NotifyAll` | ~535 行 | 洞 1、2 |
| 2 | `tools/call` 交给 `mcp.Server.AddTool` | ~140 行 | — |
| 3 | 认证改 `auth.Identity`，`Caller` 收进能力层 | ~200 行 | 洞（auth 导出） |
| 4 | `main.go` 只剩组装，daemon/CLI 交给宿主 | ~300 行 | 洞 5 |

合计约 1200 行离开 zero-memcp，剩下的全是「记忆体怎么工作」。

## 六、边界（什么不许进框架）

- 权限语义（分支、挂载、锁）——能力层的事
- 记忆的读写规则、索引、同步——业务
- 管理面板的页面——业务
- 任何「通用 CRUD」式的抽象——一旦进去，框架就开始替用户做决定

框架只负责：**把一行命令变成一次调用，把一个调用变成一次响应，把一次响应送达。**
