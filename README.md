# zero-memcp

面向 AI 编码智能体的持久记忆体。

你写下的每一条记忆先进单线程脏层写者队列，立即返回确认；写者串行将内容落成 Git blob 并更新对应目录的脏覆盖。
进程强杀或异常重启后，未封箱的脏层条目依然可读；已提交历史通过内容寻址与 Merkle 树保持不可变。

## 核心设计

1. **结构中立的记忆树**：没有额外的「主题」「频道」或「消息总线」概念。树上的路径（如 `team/chat`、`notes`）即为语义域。
2. **多态原语 `put`**：
   - `put -- $TEXT`：写入默认流水记忆树，按时间自动分片；
   - `put <tree> -- $TEXT`：写入指定的子树；
   - `put <tree> --ref <addr>`：在子树中追加已有记忆的引用，不重复复制正文。
3. **指令与数据彻底分离**：严格遵循 Termcp 规范，`cmd` 只包含指令骨架，真实正文通过 `vars` 或 `args` 传入。未引用的变量直接报错，彻底避免正文拼接引发的命令注入与断句失真。
4. **子树锚定与差分感知**：
   - `remind [<tree>]`：将 Agent 的注意力锚定在特定子树上；
   - 当该子树发生提交演进时，SSE 精准广播树更新事件；
   - `since <tree> <commit>`：Agent 醒来后比对两次提交之间的子树差异，O(1) 忽略未修改的子树，只取回实际变动的切片。
5. **写入只投递，封箱由策略决定**：
   - `put` 返回只表示「已进入单线程脏层写者队列」，不等待落盘也不提交；
   - 折叠成 Git 历史由显式 `commit` 或后台 `[commit]` 策略（条数/时间）触发；
   - 内存脏层与不可变 Git 树分属两层，崩溃只会丢掉尚未落盘的脏文件，不会破坏已提交历史。
6. **记忆同步**：
   - `sync` / `fetch` / `push`：节点与远端之间的记忆比对、取回与推送，强制显式指定分支且禁止强推。
   - `fetch` / `push` / `sync` / `gc` 全部由内置实现完成，**不依赖系统 Git**：SSH 走 Go SSH，HTTP(S) 走 smart-HTTP（自动协商协议 v2），本地裸仓库路径直接读写对象库；`gc` 用自研打包器生成 pack + `.idx` v2，`git fsck` 校验通过。

## 快速上手

### 1. 编译与启动

```bash
go build -o zero-memcp ./cmd/zero-memcp
./zero-memcp -data ~/.zero-memcp -listen 127.0.0.1:1729
```

### 2. 通过 Termcp 协议调用

```json
{
  "name": "term",
  "arguments": {
    "cmd": "put team/chat -- $TEXT",
    "vars": {
      "TEXT": "今天完成记忆体核心架构收敛：纯粹的 Merkle 树模型，取消独立总线。"
    }
  }
}
```

```json
{
  "name": "term",
  "arguments": {
    "cmd": "recall $1",
    "args": ["记忆体核心架构"]
  }
}
```

### 3. 查看树增量

```json
{
  "name": "term",
  "arguments": {
    "cmd": "since team/chat a1b2c3d4e5f6..."
  }
}
```

## DSL 原语一览

| 动词 | 用法 | 说明 |
|---|---|---|
| `put` | `put [<tree>] -- $TEXT` | 写入正文，投递至脏层并立即返回确认 |
| `put` | `put <tree> --ref <addr>` | 追加已有记忆的稳定指针 |
| `recall` | `recall <text> [--scope <tree>]` | 语义联想与回想，返回内容摘要与匹配度 |
| `read` | `read <addr> [...]` | 细读一个或多个具体切片 |
| `grep` | `grep <pattern> [--scope <tree>]` | 全文正则精确检索（符号、IP、代码名） |
| `log` | `log [N]` | 时间线追溯 |
| `ls` | `ls [date]` | 按日期列出切片条目 |
| `trees` | `trees <project>[@<branch>]` | 查看子树目录结构 |
| `remind` | `remind [<tree>]` | 查看或锚定注意力到特定子树 |
| `since` | `since <tree> <commit>` | 取回自某次提交以来的子树差分 |
| `sync` | `sync <project> [<remote>]` | 检查与远端的记忆领先/落后差异 |
| `fetch` | `fetch <project> [<remote>]` | 取回远端历史（不改动本地活动分支） |
| `push` | `push <project> [<remote>] <branch>` | 显式 SSH 推送一个获准分支（内置客户端） |
| `remote-key` | `remote-key list/put/delete` | root 管理仅服务端可读的 SSH 私钥 |
| `remote-secret` | `remote-secret list/put/delete` | root 管理仅服务端可读的 HTTP token |
| `status`| `status` | 查看记忆索引与文档规模 |
| `help` | `help [<verb>]` | 动词手册与用法指南 |

### SSH 远端推送（当前最小闭环）

私钥通过 root 凭证的 `vars` 安全地与命令分离后写入服务端 `system/sshkeys/`（目录 `0700`、文件 `0600`），私钥内容不会回显：

```json
{
  "name": "term",
  "arguments": {
    "cmd": "remote-key put github-deploy -- $KEY",
    "vars": { "KEY": "-----BEGIN OPENSSH PRIVATE KEY-----\n..." }
  }
}
```

远端声明绑定私钥名及预先核验的 SSH host fingerprint：

```toml
[[remote]]
project  = "blackegg/daily"
name     = "github"
url      = "git@github.com:org/daily.git"
ssh_key  = "github-deploy"
host_key = "SHA256:<预先核验的指纹>"
push     = ["zero"]
```

一次 `push` 只接受一个分支；未加密 OpenSSH PEM 私钥；拒绝非快进；SSH host key 必须匹配 pin。

### HTTPS 远端推送

token 同样由 root 写入服务端密钥库，配置只引用名字，token 本身不进 TOML、不进 URL：

```json
{
  "name": "term",
  "arguments": {
    "cmd": "remote-secret put gh -- $TOKEN",
    "vars": { "TOKEN": "ghp_..." }
  }
}
```

```toml
[[remote]]
project    = "blackegg/daily"
name       = "github"
url        = "https://github.com/org/daily.git"
http_user  = "x-access-token"
http_token = "gh"
push       = ["zero"]
```

明文 `http://` 只对回环地址放行——token 不能被发到网络上。

### 把外部历史推进来

注入一份已经成形的历史（另一台机器的备份、一次批量导入）不该被拆成几千条 `put`。声明一个允许 `git-receive-pack` 的端口，就能用原生 `git push` 推进来：

```toml
[[listen]]
addr  = "0.0.0.0:1731"
allow = ["git-upload-pack", "git-receive-pack"]
```

```bash
git push http://zero:<token>@host:1731/repos/daily.git HEAD:refs/heads/import
```

四条边界：

- **只接受快进**。非快进会被拒绝并说明原因；覆盖已有历史要走 `reset`（root 显式动作），不走 push。
- **只接受 `refs/heads/*`**，不接受删除分支（`delete-refs` 没有广告）。
- **凭证必须有对应分支的写权限**，端口没声明 `git-receive-pack` 时直接 403。
- **落地即重建索引**，并广播树脉冲——外部推进的 ref 不会变成静默写入。

推到一个新分支之后再决定要不要采纳：

```
reset daily@zero from daily@import
```

协议细节：取回优先用 v2（`ls-refs` + `fetch`），服务端不支持时退回 v0；协商时带本地分支尖作为 `have`，避免每次拉完整历史；v2 的 packfile 段按 side-band-64k 解复用（0x01 数据 / 0x02 进度 / 0x03 错误）。

推送与取回都支持三种远端：`ssh://git@host/org/repo.git`（或 `git@host:org/repo.git`）、`https://host/org/repo.git`、以及本地裸仓库路径（备份盘、挂载卷）。本地路径写的是对方的对象库，不参与对方进程的锁，适合「目标是一份安静的备份」。
