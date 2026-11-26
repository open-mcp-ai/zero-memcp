#!/bin/sh
# 从 GHCR 的国内镜像源拉取指定 tag，校验 digest 与 ghcr.io 官方一致后，
# 再打上 ghcr.io 的标签。
#
# 为什么需要这一步：rock64 到 pkg-containers.githubusercontent.com（ghcr 的
# 实际 blob CDN）只有约 20KB/s，4.4MB 的二进制层要三分多钟，dockerd 表现为
# 「Download complete 之后再也不动」。而 docker.io 走 registry-mirrors 是
# 8MB/3s。
#
# 镜像源不是第二个来源：南大镜像按 digest 原样代理 ghcr。所以这里先向
# ghcr.io 要官方的 index digest，再拿镜像源拉回来的 digest 去比——不相等
# 就拒绝打标签。docker-compose.yml 里仍然只写 ghcr.io 的镜像名。
set -eu

TAG="${1:?用法: zm-image-sync.sh <tag>  例如 v0.2.7}"
REPO="open-mcp-ai/zero-memcp"
SRC="ghcr.nju.edu.cn/${REPO}"
DST="ghcr.io/${REPO}"
# docker 的路径与 socket 因机器而异，这里不替人决定：
#   - rock64 是自建 dockerd，socket 在 /tmp/docker.sock
#   - macOS 用 Docker Desktop，socket 在 ~/.docker/run/docker.sock
# 所以只补默认值，不覆盖调用方已给的值，也不硬编码 PATH。
if [ -z "${DOCKER_HOST:-}" ] && [ -S /tmp/docker.sock ]; then
    DOCKER_HOST="unix:///tmp/docker.sock"
    export DOCKER_HOST
fi
# rock64 上 dockerd 不在常规 PATH 里
if ! command -v docker >/dev/null 2>&1 && [ -x /opt/dockerd/bin/docker ]; then
    PATH="/opt/dockerd/bin:$PATH"
    export PATH
fi

# 1. 向 ghcr.io 要官方 digest（manifest 请求很快，慢的只是 blob 下载）
echo "→ 取 ghcr.io 官方 digest"
TOKEN=$(curl -sS -m 30 "https://ghcr.io/token?scope=repository:${REPO}:pull" \
        | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
OFFICIAL=$(curl -sS -m 30 -o /dev/null -D - \
        -H "Authorization: Bearer ${TOKEN}" \
        -H "Accept: application/vnd.oci.image.index.v1+json" \
        -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
        "https://ghcr.io/v2/${REPO}/manifests/${TAG}" \
        | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:" {print $2}')
if [ -z "$OFFICIAL" ]; then
    echo "✗ 拿不到 ghcr.io 的官方 digest，无法校验" >&2
    exit 1
fi
echo "  官方 digest: $OFFICIAL"

# 2. 从镜像源拉（快）
echo "→ 从镜像源拉取 ${SRC}:${TAG}"
docker pull "${SRC}:${TAG}" >/dev/null

# 3. 比对
LOCAL=$(docker images --digests --format '{{.Repository}}:{{.Tag}} {{.Digest}}' \
        | awk -v k="${SRC}:${TAG}" '$1==k {print $2}')
if [ "$LOCAL" != "$OFFICIAL" ]; then
    echo "✗ digest 不一致，拒绝打标签" >&2
    echo "  镜像源: ${LOCAL:-<none>}" >&2
    echo "  官方  : ${OFFICIAL}" >&2
    exit 1
fi
echo "✓ digest 与 ghcr.io 一致"

# 4. 打上 ghcr.io 的标签，compose 就能用官方名字
docker tag "${SRC}:${TAG}" "${DST}:${TAG}"
echo "✓ ${DST}:${TAG} 就绪"
