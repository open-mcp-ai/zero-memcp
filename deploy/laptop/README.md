# laptop（权威节点）容器部署

## 这个目录解决什么

本机是权威写节点。它从镜像启动的理由不是「省事」，而是**升级路径统一**：
三个节点都跑同一个 OCI 镜像，升级就是 pull 一个新 tag，不再有
「交叉编译三个平台 → 传到三台机器 → 各自替换二进制 → 校验 SHA256」
这条每次都可能踩坑的流程。

## 前置

- Docker Desktop（macOS）。`docker info` 必须能返回 ServerVersion。
- 镜像拉取见 `../backup-node/zm-image-sync.sh`：从 ghcr.io 的国内镜像源取，
  再按 ghcr.io 的官方 index digest 校验，最后打上 `ghcr.io/...` 标签。
  本机直连 ghcr.io 实测约 37KB/s，一个 20MB 镜像要十分钟以上；
  走镜像源 5 秒。

## 配置从哪来

`zero-memcp.container.toml` **由宿主机的 `~/.zero-memcp/zero-memcp.toml`
派生**，只改 5 处，不手写第二份：

| 宿主机 | 容器内 | 为什么 |
|---|---|---|
| `data = "/Users/<user>/.zero-memcp/data"` | `data = "/data"` | 挂载点 |
| —（无） | `enforce_auth = true` | 非回环监听必须带凭证 |
| `addr = "127.0.0.1:1729"` | `addr = "0.0.0.0:1729"` | 见 compose 注释第 1 条 |
| —（无） | `require = "bearer"` | 同上 |
| `vectors = "<data>/vectors"` | `vectors = "/data/vectors"` | 挂载点 |
| `endpoint = "http://127.0.0.1:11434"` | `endpoint = "http://host.docker.internal:11434"` | 容器里的 127.0.0.1 是容器自己 |

派生而不是手抄，是因为**配置会漂**：本机配置改了一条 remote、加了几个
分支，手抄的第二份不会跟着变，然后在某个凌晨表现为「同步静默落后」。
生成脚本见 `gen-container-config.py`。

## 切换步骤（含回滚）

```bash
# 1. 取镜像并校验 digest
../backup-node/zm-image-sync.sh v0.2.8

# 2. 从宿主机配置派生容器配置
python3 gen-container-config.py

# 3. 记下当前各分支内容树（回滚后逐条比对）
#    见 README 顶部脚本说明

# 4. 停宿主机进程 —— 必须停：两者共用同一 data 目录与 instance.lock
pkill -f 'zero-memcp serve'
sleep 4

# 5. 起容器
docker compose up -d

# 6. 验：health / 13 条分支三端内容树 / srecall / 插件 token
curl -sS http://127.0.0.1:1729/health

# 回滚：docker compose down，然后
#   ~/.local/bin/zero-memcp serve -config ~/.zero-memcp/zero-memcp.toml
```

## 容器化后的行为变化（须知）

1. **本机免签降级消失**。`auth_middleware.go` 的 `isLocal()` 判
   `RemoteAddr` 前缀 `127.0.0.1`，经端口映射进来的请求源地址是 Docker
   网关 IP。所以所有客户端都必须带凭证。
   - pi 插件：已带（`~/.zero-memcp/config.json` 的 `tokens.weft` / `tokens.zero`）
   - 运维脚本：已带 admin token
   - WebUI：需要登录（首次启动日志里会给一次性密码，或用 token）
2. `whoami` 的答案会变：`loopback-exempt` 时本机调用降级成 root；
   带 token 时是 token 上的身份与挂载表。这更正确，但**行为变了**。
3. 嵌入服务走 `host.docker.internal`，实测 6ms，与原生无差别。
