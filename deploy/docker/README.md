# zero-memcp 标准容器部署（推荐）

## 为什么优先容器化

1. **统一镜像，免平台差异**：一条 `docker pull` + `docker compose up -d` 覆盖 amd64 / arm64（macOS、Linux、ARM 小盒子），不再有交叉编译、覆盖运行中二进制、文件权限位丢失的问题。
2. **数据只落在挂载卷**：记忆树（`repos/`）、系统表与向量全在宿主机 `/data` 绑定目录；升级或回滚只换镜像 tag，不动数据。

## 快速开始

```bash
# 1. 准备数据目录与配置（字面照抄；这份示例已经按容器改好了三处）
mkdir -p ~/.zero-memcp/data
cp zero-memcp.container.example.toml ~/.zero-memcp/zero-memcp.toml

# 2. 启动（compose 默认就以 ~/.zero-memcp/zero-memcp.toml 为配置）
docker compose up -d

# 3. 验证（应回 {"ok":true,"version":"..."}）
curl -sS http://127.0.0.1:1729/health
```

首次启动会在日志里打印一次性管理口令，登录 WebUI 后必须修改：

```bash
docker compose logs | grep -i password
```

**已经有了宿主机原生配置？** 用 `derive-container-config.py` 派生，
不要手抄第二份——手抄的那份会漂，漂了的症状是「同步静默落后」：

```bash
python3 derive-container-config.py \
  --src ~/.zero-memcp/zero-memcp.toml \
  --dst ~/.zero-memcp/zero-memcp.container.toml
ZERO_MEMCP_CONFIG=~/.zero-memcp/zero-memcp.container.toml docker compose up -d
```

> 中国大陆网络直连 `ghcr.io` 的 blob CDN 速度较慢时，可用 `../backup-node/zm-image-sync.sh <tag>` 从国内镜像源拉取并校验官方 index digest 后再打回 `ghcr.io` 标签。

## 从宿主机原生配置派生容器配置

`derive-container-config.py` 会从宿主机 `zero-memcp.toml` 按固定规则替换四处容器内差异，避免手抄两份配置逐渐漂移：

| 宿主机 | 容器内 | 原因 |
|---|---|---|
| `data = "<host_data>"` | `data = "/data"` + `enforce_auth = true` | 挂载点 + 端口映射必开鉴权 |
| `addr = "127.0.0.1:1729"` | `addr = "0.0.0.0:1729"` + `require = "bearer"` | 容器端口转发连的是 `eth0`，连不到 `lo` |
| `vectors = "<host_data>/vectors"` | `vectors = "/data/vectors"` | 向量缓存挂载点 |
| `endpoint = "http://127.0.0.1:11434"`（可选） | `endpoint = "http://host.docker.internal:11434"` | 容器内的 `127.0.0.1` 是容器自身，访问宿主机 Ollama 走 `host.docker.internal` |

## 容器化的三个行为变化（须知）

1. **端口安全边界**：容器内监听 `0.0.0.0:1729`，但 `docker-compose.yml` 默认只映射到宿主机 `127.0.0.1:1729:1729`。若需对局域网暴露，通过 `ZERO_MEMCP_BIND=0.0.0.0:1729` 覆盖。
2. **必须携带 Bearer Token**：经端口映射进来的连接源地址是 Docker 网关 IP，原「本机回环免签降级成 root」失效。请在配置里开启 `enforce_auth = true` 与 `require = "bearer"`，客户端携带签发好的 token。
3. **升级与回滚**：改 `ZERO_MEMCP_VERSION`（或 `docker-compose.yml` 里的镜像 tag）后执行 `docker compose up -d`；旧镜像留在本地，回滚只需改回旧 tag 再 `up -d`。

## 迁移已有宿主机进程到容器

若宿主机上正在跑原生 `zero-memcp serve`：两者共用同一份 `data/` 与 `instance.lock`，必须先停宿主机进程再起容器：

```bash
pkill -f 'zero-memcp serve' && sleep 3
docker compose up -d
```
