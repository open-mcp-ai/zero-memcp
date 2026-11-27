# 升级流程：容器优先

## 原则

**升级 = 改一个镜像 tag，然后 `docker compose up -d`。**

不再有「本地交叉编译 → 传到各台机器 → 替换二进制 → 校验 SHA256」这条流程。
它每次都可能踩坑（传输丢 `+x`、覆盖写运行中的文件、Windows 的 GBK 与 BOM），
而它带来的唯一好处是「不出网」——这个好处由镜像与 digest 校验替代了。

数据（`repos/`、`system/`、`vectors/`）全在宿主机的绑定目录里，换镜像不碰数据。

## 一次升级

```bash
cd deploy/docker

# 1. 改版本（或在 .env 里写 ZERO_MEMCP_VERSION=v0.2.10）
export ZERO_MEMCP_VERSION=v0.2.10

# 2. 拉取 + 重建（旧镜像留在本地，回滚不用重新下载）
docker compose pull && docker compose up -d

# 3. 验版本
curl -sS http://127.0.0.1:1729/health
# {"auth":"enforced","ok":true,"service":"zero-memcp","version":"v0.2.10",...}
```

回滚：把 tag 改回去再 `up -d`。**数据不会被旧版本改坏**——索引快照是派生物，
版本不匹配时会自动弃用并重建（见下）。

## 升级后第一次启动会重建索引

索引快照带版本指纹；指纹变化时启动日志会明写：

```
reload: index scope changed ("" -> "inc=;exc="); discarding snapshot
building initial index (reading every branch of every project)
```

这段是**一次性成本**，几分钟量级（取决于块数与磁盘）。两个注意点：

1. **确认 `[index] async` 在生效**。默认是开（进程先服务、索引后台建、`status` 报进度）。
   v0.2.10 之前存在一个加载器缺陷：`[index]` 段里没写 `async` 时，默认值被读成
   `false`，于是变成「listen 之前同步重建全量索引」，端口几分钟不监听。
   升级到含该修复的版本即可；更早的版本请显式写 `async = true`。
2. **重建期间 `recall` 只覆盖已建好的部分**。`status` 首行会报
   `index: building 12000/28000 blocks (42%)`；`read` / `ls` / `log` / `grep`
   不受影响（它们读仓库本身）。

## 发布新版本（CI 侧）

```bash
git tag v0.2.10 && git push origin v0.2.10
```

`.github/workflows/docker.yml` 在 tag 上构建 `linux/amd64` 与 `linux/arm64` 并推
`ghcr.io/open-mcp-ai/zero-memcp:v0.2.10`。等 docker workflow 绿了再升级各节点。

## 受限网络：从镜像源取，但按官方 digest 校验

直连 `ghcr.io` 的 blob CDN 在某些网络下极慢（实测 20–40KB/s，一个 20MB 镜像
要十分钟以上，dockerd 表现为「Download complete 之后再也不动」）。此时用
`backup-node/zm-image-sync.sh`：

```bash
./backup-node/zm-image-sync.sh v0.2.10
```

做法是**先向 ghcr.io 要官方 index digest，再从镜像源拉，两边 digest 不相等就拒绝打标签**。
镜像源不是第二个来源——它按 digest 原样代理，所以 compose 里仍然只写 `ghcr.io/...`。

## 容器化的三个必知事项

这三条都是实测出来的，改配置时容易踩：

1. **容器内必须监听 `0.0.0.0`**。容器里只绑 `127.0.0.1` 时，宿主经端口映射拿到的是
   `Empty reply from server`——Docker 的转发连的是容器 `eth0`，连不上它的 `lo`。
   宿主侧仍只映射 `127.0.0.1:1729:1729`，那个 `0.0.0.0` 不是对外监听面。
2. **容器化会失去本机免签降级**。`isLocal()` 判 `RemoteAddr` 前缀 `127.0.0.1`，
   经端口映射进来的源地址是 Docker 网关 IP。所以配置里要有 `enforce_auth = true`
   + `require = "bearer"`，所有客户端都带凭证。`whoami` 的答案会从「降级成 root」
   变成 token 上的身份与挂载表——这更正确，但行为变了。
3. **嵌入服务地址要改**。容器里的 `127.0.0.1:11434` 是容器自己，宿主上的 Ollama
   要写 `host.docker.internal:11434`（compose 里已有 `extra_hosts`）。

## 多节点

每个节点跑同一个镜像，差异只在配置文件（数据目录、项目、远端、索引开关）。
`deploy/docker/derive-container-config.py` 可以从一份宿主机配置派生容器配置，
避免手抄两份逐渐漂移——手抄的那份会漂，漂了的症状是「同步静默落后」，最难查。
