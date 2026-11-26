# 升级流程：改镜像版本号，直接拉取

## 原则

**所有节点都从 CI 构建的镜像启动。** 升级 = 改一行版本号 + 拉取 + 重建容器。

不再有「本地交叉编译 → 传到三台机器 → 各自替换二进制 → 校验 SHA256」
那条流程。它每次都可能踩坑（termcp 传输丢 `+x`、覆盖写已有文件在 rename
阶段失败、Windows 的 GBK 与 BOM），而它带来的唯一好处是「不出网」——
这个好处由 GHCR 镜像与 digest 校验替代了。

## 为什么拉取要走脚本，不直接 `docker pull ghcr.io/...`

实测（2026-11-26）：

| 路径 | 4.4MB 的二进制层 |
|---|---|
| rock64 → ghcr 的 blob CDN | 240 秒只到 1.87MB（**约 20KB/s**） |
| 本机 Mac → 同一条路径 | 45 秒到 1.66MB（**约 37KB/s**） |
| rock64 → docker.io（走 registry-mirrors） | 8MB / **3 秒** |

`/opt/dockerd/daemon.json` 里的 `registry-mirrors` **只对 docker.io 生效**，
管不到 ghcr。所以 dockerd 会显示 `Download complete` 之后长时间不动，
看起来像卡死，其实是在以 20KB/s 爬。

`zm-image-sync.sh` 的做法：**先向 ghcr.io 要官方的 index digest，再从国内
镜像源拉，两边 digest 不相等就拒绝打标签。**

镜像源不是第二个来源——南大镜像按 digest 原样代理 ghcr。实测 v0.2.8：
镜像源拉回来的 repo digest 与 ghcr.io 的 index digest 逐字节相同。
`docker-compose.yml` 里仍然只写 `ghcr.io/...`，脚本只负责快速取货。

实测耗时：全新 tag 拉取 + 校验 + 打标签 **10–15 秒**。

## 谁从镜像启动

| 节点 | 状态 | 镜像 | 管理方式 |
|---|---|---|---|
| laptop | ✅ 容器 | `ghcr.io/open-mcp-ai/zero-memcp:v0.2.8` | `deploy/laptop/docker-compose.yml` |
| rock64 | ✅ 容器 | `ghcr.io/open-mcp-ai/zero-memcp:v0.2.8` | `/mnt/private/deploy/zero-memcp/docker-compose.yml` |
| 3060 | ⬜ 仍是原生 exe | — | 未动（需提前报备） |

## 升级一个节点

```bash
# 1. 拉新版本并校验 digest
/path/to/zm-image-sync.sh v0.2.9

# 2. 改 compose 里的版本号（一个字符串）
sed -i 's|zero-memcp:v0.2.8|zero-memcp:v0.2.9|' docker-compose.yml

# 3. 重建
docker compose up -d

# 4. 验版本
curl -sS http://<host>:1729/health
```

回滚就是把版本号改回去再 `up -d`——旧镜像还在本地，不用重新下载。

## 发布新版本（CI 侧）

```bash
git tag v0.2.9 && git push origin v0.2.9
```

`.github/workflows/docker.yml` 会在 tag 上构建 linux/amd64 与 linux/arm64
两个架构并推到 `ghcr.io/open-mcp-ai/zero-memcp:v0.2.9`。等 `docker`
workflow 绿了再升级各节点。

## 容器化的三个必知事项

这三条都是实测出来的，改配置时容易踩：

1. **容器内必须监听 `0.0.0.0`**。容器里只绑 `127.0.0.1` 时，宿主经端口
   映射拿到的是 `Empty reply from server`——Docker Desktop 的转发连的是
   容器 `eth0`，连不上它的 `lo`。宿主侧仍只映射 `127.0.0.1:1729:1729`，
   那个 `0.0.0.0` 不是对外监听面。
2. **容器化会失去本机免签降级**。`auth_middleware.go` 的 `isLocal()` 判
   `RemoteAddr` 前缀 `127.0.0.1`，经端口映射进来的源地址是 Docker 网关 IP。
   所以配置里要有 `enforce_auth = true` + `require = "bearer"`，所有客户端
   都带凭证。`whoami` 的答案会从「降级成 root」变成 token 上的身份。
3. **嵌入服务地址要改**。容器里的 `127.0.0.1:11434` 是容器自己，
   宿主 ollama 要写 `host.docker.internal:11434`（compose 里加
   `extra_hosts: host.docker.internal:host-gateway`）。实测 6ms，与原生无差别。

laptop 的容器配置由 `deploy/laptop/gen-container-config.py` 从宿主机
`~/.zero-memcp/zero-memcp.toml` **逐行派生**，不手写第二份——手抄的那份
会漂，漂了的症状是「同步静默落后」，最难查。
