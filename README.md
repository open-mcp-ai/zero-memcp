# zero-memcp

面向 AI 编码智能体的持久记忆体，以 Git 裸仓库为物理存储、以 MCP 为唯一入口。

**工作层是主体，commit 是历史书签。** 两台机器写完互相可见，不需要先 commit；
commit 只是"此刻在此处拍了一张照"，给外部 Git 工具与人审计用。

---

## 核心设计

1. **结构中立的记忆树**
   没有"主题""频道""消息总线"这类额外概念。树上的路径（`team/chat`、`notes`）就是语义域。

2. **工作层同步，不依赖 commit**
   写入先落工作层（`dirty/<branch>/@<dir>.dirty`，纯文本可 `cat`/`diff`）。
   节点间直接同步这一层：先 tree 后 blob，删除优先落地，被删 blob 永远不会再被请求。
   commit 把工作层折叠成 Git 历史，是快照而非同步前提。

3. **指令与数据彻底分离**
   `cmd` 只放指令骨架，正文一律通过 `vars`/`args` 以变量引用传入。
   内联正文会被协议直接拒绝——正文里的 `;` 就是分号，不会被当成命令分隔符。

4. **单工具，能力在命令空间**
   MCP 只暴露一个 `term` 工具。能力增长发生在动词层，不在 `tools/list`，
   所以上下文占用恒定。

5. **权限落在端口、配置、凭证三层**
   任一层说不行就是不行，凭证只能收窄不能放大。root 只由 `[[user]] root = true` 决定。

6. **派生物不当真相**
   BM25 倒排、索引快照、向量索引都是可重建的缓存；Git 才是真相。
   快照与现状不一致时**治愈**（差量补丁 + 逐条对齐），不整份弃用。

---

## 快速上手

### 容器（推荐）

```bash
mkdir -p ~/.zero-memcp/data
cp zero-memcp.example.toml ~/.zero-memcp/zero-memcp.toml
cd deploy/docker
docker compose up -d
curl -sS http://127.0.0.1:1729/health
```

镜像同时提供 `linux/amd64` 与 `linux/arm64`；升级就是换 `ZERO_MEMCP_VERSION`
再 `docker compose up -d`，数据在宿主绑定目录里不动。细节与回滚见
[`deploy/docker/README.md`](deploy/docker/README.md)、
[`deploy/UPGRADING.md`](deploy/UPGRADING.md)。

### 编译（原生二进制）

```bash
go build -o zero-memcp ./cmd/zero-memcp            # 默认：http + static 嵌入
go build -tags onnx ./cmd/zero-memcp               # 追加 ONNX（需 CGO + libonnxruntime）
```

`zero-memcp version` 会打印本二进制编译进了哪些嵌入能力。

### 启动（原生）

```bash
zero-memcp serve -data ~/.zero-memcp/data -config ~/.zero-memcp/zero-memcp.toml
# 兼容旧写法：
zero-memcp -data ~/.zero-memcp/data -listen 127.0.0.1:1729
```

首次启动会生成签名密钥与一次性管理口令，口令打印一次到 stderr，登录后必须修改。

### 调用（MCP）

```json
{
  "name": "term",
  "arguments": {
    "cmd": "put team/chat $TEXT",
    "vars": { "TEXT": "今天收敛了工作层同步：不再依赖 commit。" }
  }
}
```

```json
{
  "name": "term",
  "arguments": { "cmd": "recall $1", "args": ["工作层同步"] }
}
```

---

## 常用动词

| 动词 | 用法 | 说明 |
|---|---|---|
| `put` | `put [<tree>] $TEXT` | 写入正文，投递到工作层并立即返回 |
| `ref` | `ref <tree> <addr>` | 追加已有记忆的引用，不复制正文 |
| `recall` | `recall <q> [--scope T] [--json]` | BM25 检索；配置嵌入后端后可用 `srecall` |
| `read` | `read <addr> [...]` | 细读具体切片 |
| `grep` | `grep <pat> [--scope T]` | 全文正则，符号/IP/代码名精确命中 |
| `log` / `ls` | `log [N]` / `ls [date]` | 时间线与按日期列举 |
| `trees` | `trees <project>[@<branch>]` | 查看树结构 |
| `remind` / `since` | `remind <tree>` / `since <tree> <commit>` | 关注某棵树并取回差量 |
| `status` | `status` | 索引规模、冲突、同步水位、缓存占用 |
| `commit` | `commit [<p>[@<br>]] [-m msg]` | 把工作层折叠成历史书签 |
| `write` | `write <p>@<br>/<path> $TEXT` | 精确路径写入 |
| `apply` | `apply <p>[@<br>] $PATCHES` | 一次往返多文件改写 |
| `sync` / `fetch` / `push` / `merge` | 见 `help` | 内置传输，不依赖系统 Git |
| `help` | `help [<verb>]` / `help --all` | 动词手册 |

维护类动词（`pending`/`diff`/`branch`/`tag`/`cp`/`mv`/`delete`/`archive`/`reset`/
`lock`/`unlock`/`chmod`/`erase`/`replace`/`conflict`）见 `help --all`。

### 一条命令跑多条语句

```
recall q --scope p@fact || recall q --scope p@record || recall q --scope p@thought
```

`;` 顺序执行，`&&` 上一条**查到内容**才继续，`||` 上一条没查到才继续。
判据是"命中"而非退出码——搜不到不是错误，真正的错误（语法、权限）一律中断整条链。

---

## 权限模型

| 层 | 位置 | 作用 |
|---|---|---|
| 端口 | `[[listen]] allow / mount / require` | 这个 socket 上存在哪些动词、看得见哪些命名空间 |
| 配置 | `[[project]] mode/owner` + `[[access]]` | 该用户对该项目的读写执行权限 |
| 凭证 | JWT 的 `cap` / `mount` / `gated` | 这张纸本身的上限，只能更窄 |

**能力门（gated）**：签发时就按住其中几位，平时一律拒绝，
只有管理台按 (项目, 分支) 临时放行后才生效。授权不重签 JWT，
按 JTI + 项目 + 分支存储，到期或撤销后下一次请求即失效。

**注意**：`enforce_auth = false`（默认）时，本机任意进程不带凭证即被视为 root。
多用户机器或容器里必须打开。

---

## 管理台（WebUI）

MCP 管记忆存取；凭证签发、临时授权、对象打包这类运维动作在管理台做。

- 与 MCP 共用 http.Server，但鉴权完全分离：两种凭据**互相拒绝**；
- 管理台一定存在，不是开关能关掉的（容器里拿不到命令行，"默认关闭"等于把人锁在门外）；
- 端口只决定它挂在哪儿：单端口就是入口；多端口没写 `webui = true` 时落在第一个；
  写了就只挂在那一个上，其余端口的管理路由根本不存在（404 而非 403）；
- 口令以 argon2id 存在 `system/ui.passwd`，不进 TOML（`config show` 会回显给 Agent）。

CLI 因此只剩 `serve` / `daemon` / `version` / `help`：装配与诊断，不是客户端。

---

## 同步

两种远端，按 URL 形状分流：

| 远端形态 | 走什么 | 是否需要 commit |
|---|---|---|
| `http(s)://` 且不以 `.git` 结尾（另一个 zero-memcp） | 工作层同步 `/sync/work/*` | **不需要** |
| 其他（GitHub、本地裸仓库、SSH） | Git fetch/merge/push | 需要先提交 |

```toml
[[remote]]
project   = "blackegg/daily"
name      = "hub"
url       = "http://10.10.10.12:1729"
push      = ["zero"]
auto_sync = true      # 不写就不进后台循环
on_receive = false    # 依赖 auto_sync
```

四条纪律：不自动 push（除显式 `auto_sync`）、不隐式 commit、
不强推、冲突不静默（保留本地并上报，`status` 点名）。

SSH 私钥与 HTTP token 由 root 写入服务端密钥库，配置只引用名字；
明文 `http://` 只对回环地址放行。

---

## 文档

| 文件 | 内容 |
|---|---|
| [`docs/FEATURES.md`](docs/FEATURES.md) | **能力矩阵与已知限制**（以代码为准，标注验证状态） |
| [`docs/DESIGN.md`](docs/DESIGN.md) | 架构决策记录：为什么不用那些更省事的做法 |
| [`docs/DESIGN-WORKLAYER.md`](docs/DESIGN-WORKLAYER.md) | 工作层同步协议、水位、抹除与审计 |
| [`zero-memcp.example.toml`](zero-memcp.example.toml) | 带注释的完整配置样例 |

---

## 构建与测试

```bash
make test      # 常规测试
make perf      # 性能回归（//go:build perf）
make check     # vet + 常规测试
make all       # vet + test + perf
make cross     # 六平台交叉编译
```

无锁纪律（禁止 `sync.Mutex` / `sync.RWMutex`）由 `internal/guard` 在常规测试里检查，
不需要单独目标——跑 `make test` 就会执行。

CI 在三个地方跑：`ubuntu-latest`（vet + 全量测试）、
`windows-latest`（gitstore + server）、六平台交叉编译。

**平台差异必须在真平台验证**：Windows 上曾有四处真实缺陷
（路径归一化、密钥权限位、本地远端路径、句柄释放）只在真机暴露。
