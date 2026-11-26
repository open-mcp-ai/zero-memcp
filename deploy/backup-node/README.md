# 备份节点部署指南

## 拓扑

```
laptop (本机, 权威)          rock64 (10.192.168.164, 纯备份)
  BlackEggAI/zero  ──push──▶   BlackEggAI/zero
  BlackEggAI/weft  ──push──▶   BlackEggAI/weft

3060 (hub, 常开)  ──push──▶   （同构，后续追加）
```

**备份节点只推不拉**（`push_only = true`）。它自己不产生内容，也不主动拉谁——
只被推、被读。恢复时由恢复方主动 `pull`。

## 一键部署（rock64）

```bash
# 1. 取镜像。
#
#    不要直接 docker pull ghcr.io/...：rock64 到 ghcr 的 blob CDN
#    （pkg-containers.githubusercontent.com）实测只有约 20KB/s，4.4MB 的
#    二进制层要三分多钟，dockerd 表现为「Download complete 之后再也不动」。
#    而经国内镜像源是 5~10 秒。
#
#    镜像源不是第二个来源：脚本会先向 ghcr.io 要官方的 index digest，
#    再拿镜像源拉回来的 digest 去比，不相等就拒绝打标签。
./zm-image-sync.sh v0.2.8

# 2. 建目录
mkdir -p /mnt/private/deploy/zero-memcp/{data,conf}

# 3. 写配置（见下方 conf/zero-memcp.toml）
# 4. 写 docker-compose.yml（见下方）
# 5. 启动
docker compose up -d
```

## 配置文件

### `conf/zero-memcp.toml`

```toml
[server]
data         = "/data"
endpoint     = "rock64"
enforce_auth = true

[[listen]]
addr    = "0.0.0.0:1729"
allow   = ["*"]
require = "bearer"
webui   = true

[[user]]
name = "zero"
root = true

[[project]]
name  = "BlackEggAI/zero"
owner = "zero"
mode  = "rwx------"

[[project]]
name  = "BlackEggAI/weft"
owner = "zero"
mode  = "rwx------"

[commit]
interval = "5m"
count    = 500

[sync]
interval = "0s"   # 纯备份节点不主动拉谁
```

### `docker-compose.yml`

```yaml
services:
  zero-memcp:
    image: ghcr.io/open-mcp-ai/zero-memcp:v0.1.1-backup
    container_name: zero-memcp-backup
    restart: unless-stopped
    ports:
      - "1729:1729"
    volumes:
      - ./data:/data
      - ./conf/zero-memcp.toml:/etc/zero-memcp/zero-memcp.toml:ro
    environment:
      - TZ=Asia/Shanghai
```

## 主机侧配置（laptop）

在 `~/.zero-memcp/zero-memcp.toml` 末尾追加：

```toml
[[remote]]
project    = "BlackEggAI/zero"
name       = "backup-rock64"
url        = "http://10.192.168.164:1729"
push       = ["main", "main.record", "main.chat", "main.dev", "main.distill", "main.me", "main.shared", "zero"]
push_only  = true
auto_sync  = true
http_user  = "zero"
http_token = "tok-backup-164"

[[remote]]
project    = "BlackEggAI/weft"
name       = "backup-rock64"
url        = "http://10.192.168.164:1729"
push       = ["main", "main.record"]
push_only  = true
auto_sync  = true
http_user  = "zero"
http_token = "tok-backup-164"
```

Token 存于 `<data>/system/secrets/tok-backup-164`（0600），由备份节点的
`jwt.key` 签发。

## 自动备份触发时机

后台同步循环 `[sync] interval = "5m"` 每 5 分钟跑一轮：
- 遍历所有 `auto_sync = true` 的远端
- 对工作层对端（URL 不以 `.git` 结尾）执行 `syncOpsWithRemote`
- `push_only` 时只推不拉

**推的是命令流（oplog），不是 Git commit**。未提交的工作层改动也会同步——
这正是备份需要的：坏了能恢复到「正在想、还没说出口」的状态。

## 恢复验证（已实测）

```bash
# 在恢复节点配置 pull 指向备份：
[[remote]]
project    = "BlackEggAI/weft"
name       = "backup-rock64"
url        = "http://10.192.168.164:1729"
pull       = ["main", "main.record"]
auto_sync  = true
http_token = "tok-backup-164"
```

实测结果：
- `main.record` 136 条命令全部恢复
- `log 3` 读出今天 22:29 的 thought 层记录
- `trees` 显示完整目录结构

## 性能实测

| 指标 | 数值 |
|---|---|
| 真实备份内容（blob 总量） | **85 MB** |
| 首次推送耗时 | ~15 分钟（逐 blob 上传，RTT 41ms） |
| ARM 板冷启动索引重建 | ~15 分钟（22,620 条） |
| 重建后内存占用 | ~830 MB |
| 后续启动（有快照） | 秒级 |

## 注意事项

1. **不要从备份节点拉取做日常同步**。`push_only` 就是为了防止这个：
   备份若只落地了半棵树（blob 缺失），回头拉会把它播种的权威整树
   （Replace）当成删除指令，清掉主机上的记忆。
2. **恢复是显式动作**：配 `pull` 指向备份，等一轮同步，然后 `reload`。
3. **CI 镜像更新**：`git push` 后 `gh workflow run docker.yml -f tag=vX.Y.Z`，
   然后在备份节点 `docker compose pull && docker compose up -d`。
