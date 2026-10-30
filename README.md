# zero-memcp

**长期记忆底座**：纯 Go、零外部依赖、以 Git 裸仓库为物理存储、WAL 断电保护、多分支隔离、支持只读 HTTP Git 与端口级安全策略。

实现 [termcp 协议](https://termcp.org)（[源码](https://github.com/open-mcp-ai/termcp)）：对外暴露单工具 `term({cmd})` 入口，声明面严格等于能力面，能力增长在命令空间，不在 `tools/list`。

---

## 为什么是 Git 裸仓库，而不是向量数据库

主流记忆方案把内容存进向量数据库，换来的是「黑盒检索」与「无法追溯」。`zero-memcp` 走向相反的方向：

- **内容不可变**：每一篇记忆是一个标准 Git blob，由 SHA-1 严格定址，不可篡改；
- **历史即提交树**：提交（Commit）把状态钉在 Merkle 树上，支持 `diff`、`branch`、`tag`、`reset` 等外科手术；
- **写与审分离**：工作 Agent 产生的脏流写在独立分支（如 `daily@chat`），由审计 Agent 提纯决议后合并入主干；
- **断电零丢失**：每条写入走 Write-Ahead Log（带 CRC32 校验），即使 `kill -9` 也不丢未提交的记录；
- **两类索引分离**：Git 树状态持久化，BM25 倒排索引纯内存派生、启动全量重建、永不落盘。

---

## 安装与快速开始

### 从源码编译

```bash
git clone https://github.com/open-mcp-ai/zero-memcp.git
cd zero-memcp
go build -o /usr/local/bin/zero-memcp ./cmd/zero-memcp
```

### 极简启动（单端口，本机开发）

```bash
zero-memcp -data ~/.zero-memcp/data -listen 127.0.0.1:1729
```

服务会自动在 `~/.zero-memcp/data/repos/` 下建立默认仓库，并开始监听。

---

## 配置文件（TOML）

生产环境推荐使用配置文件。所有安全边界均可在配置中表达：

```toml
[server]
data     = "/var/lib/zero-memcp/data"
endpoint = "edge-box"

# 定期 gc，保持松散对象整洁（可选）
[gc]
interval = "24h"

# ── 端口级策略（内核级边界）──

# 端口 1: 本机全能力（回环地址免签降级）
[[listen]]
addr  = "127.0.0.1:1729"
allow = ["*"]

# 端口 2: 只读 HTTP Git（给中心拉取记忆，需要 Token 鉴权）
[[listen]]
addr  = "0.0.0.0:1730"
allow = ["git-upload-pack"]

# 端口 3: 受限内网 MCP（只读白名单 + 强制 Token）
[[listen]]
addr    = "10.10.0.0:1731"
allow   = ["read", "ls", "recall", "grep", "status"]
require = "bearer"
mount   = ["blackegg/daily=/daily:r@chat"]

# ── 用户与项目权限 ──

[[user]]
name = "zero"
root = true

[[user]]
name   = "chat"
groups = ["agents"]

[[project]]
name  = "blackegg/me"
owner = "zero"
mode  = "rwx------"

[[project]]
name  = "blackegg/daily"
owner = "zero"
mode  = "rwx------"

[[access]]
user    = "chat"
project = "blackegg/daily"
perm    = "rw"
```

---

## 协议与使用

### 单工具入口

在 Claude Desktop、pi 或任意 MCP 宿主中配置本服务：

```json
{
  "mcpServers": {
    "zeromem": {
      "command": "zero-memcp",
      "args": ["-data", "/Users/yourname/.zero-memcp/data"]
    }
  }
}
```

模型只看得见一个工具：

```
term({ cmd: "recall 数据库选型" })
```

### 动词全表（19 个动词）

| 动词 | 用法示例 | 语义说明 |
|---|---|---|
| **读** | | |
| `recall` | `recall <text>` | BM25 语义检索（命中地址 + 摘要片段） |
| `read` | `read <addr> [<addr>...]` | 读全文，支持多地址批量读取 |
| `grep` | `grep <regex>` | 正则搜索全文（精确匹配符号、IP、代码名） |
| `log` | `log [N]` | 按时间倒序看最近 N 篇切片 |
| `ls` | `ls [date]` | 列出某天的全部切片 |
| `status` | `status` | 报告文档数、词项数、分支状态 |
| **写** | | |
| `note` | `note <kind> -- <text>` | 追加一条切片（`fact`/`episode`/`preference`/`observation`） |
| `commit` | `commit [<p>[@<br>]] [-m msg]` | 显式封箱（引擎绝不自动提交，由调用方显式提交） |
| **树外科手术**（仅 Root） | | |
| `diff` | `diff <p>@<br> [-> <p>@<br>]` | 分支比对 / 提取未审脏流 |
| `cp` | `cp <src>@<br>/<path> -> <dst>@<br>/` | 跨分支复制切片 |
| `mv` | `mv <src>@<br>/<path> -> <dst>@<br>/quarantine.md` | 跨分支剪切（支持异名搬运，如隔离区） |
| `delete` | `delete <p>@<br>/<path>` | 墓碑标记删除（需 commit 生效） |
| `batch` | `batch {"ops":[...]}` | 原子决议单（单批次执行 note/cp/mv/commit 等） |
| **生命周期**（仅 Root） | | |
| `branch` | `branch <p> <new> [from <src>]` | 分支列表 / 零拷贝分叉 |
| `tag` | `tag <p> <name> [<ref>]` | 打标签 / 读取水印 |
| `reset` | `reset <p>@<br> from <p>@<src>` | 重置分支（旧版自动归档为 `<br>-reset-*`） |
| `archive` | `archive <p>@<br>` | 归档分支（ref 移到 `refs/archived/`，不物理删除） |
| `kill` | `kill <p>@<br>` | 吊销该分支的关联凭证（子凭证随之级联失效） |
| **身份与权限** | | |
| `whoami` | `whoami` | 报告当前调用的用户、权限与有效挂载表 |
| `users` | `users` | 列出系统中配置的用户清单 |

---

## 原生 Git 互操作（只读 HTTP Git）

无需任何特殊客户端，标准 Git 可以直接拉取整棵记忆树：

```bash
# Basic Auth（密码处传 JWT Token）
git clone http://zero:<jwt>@127.0.0.1:1729/repos/blackegg/me.git

# 或带 Authorization Header
git -c http.extraHeader="Authorization: Bearer <jwt>"     clone http://127.0.0.1:1729/repos/blackegg/me.git

# 增量拉取（Fast-Forward）
cd me && git pull
```

---

## 架构保证

1. **零外部 Git 进程**：Loose Object、Packfile 读取器（含 OFS_DELTA / REF_DELTA）、Tree 生成、Commit 串行化全由 Go 标准库自研实现；产物天然符合 Git 规范，`git fsck --strict` 零报错；
2. **Crash-Safe WAL**：每次写入先写 Loose Object 临时文件并 fsync，再追加 WAL（带 CRC32 校验），最后才更新内存状态；重启时自动重放并截断半行损坏；
3. **chroot 挂载表**：Token 挂载表决定 Agent 的可见边界；挂载表外的项目完全不可见，绝对路径映射到虚拟根，输出严格虚拟化。

---

## 更多文档

- [`docs/DESIGN.md`](docs/DESIGN.md) — 架构决策记录：每个「为什么不用那个更常见的做法」
- [`zero-memcp.example.toml`](zero-memcp.example.toml) — 可直接复制的示例配置
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — 开发约定与提交规范

## 许可证

MIT License. 见 [LICENSE](LICENSE)。
