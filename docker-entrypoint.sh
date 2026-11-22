#!/bin/sh
# zero-memcp 容器入口。
#
# 存在的理由：`serve` 不会自己去找配置文件。把这件事写进入口，
# 镜像才能「挂上配置就跑」；显式传参则完全接管。
#
# **不要在这里默认 -listen 0.0.0.0**：server.go 对裸 -listen 有硬检查，
# 非回环地址会直接拒绝启动（"refusing to bind to a non-loopback address"）。
# 想让容器可从宿主机访问，正确做法是在挂载的配置里声明：
#
#   [[listen]]
#   addr    = "0.0.0.0:1729"
#   allow   = ["*"]
#   require = "bearer"     # 非回环必须带凭证，否则等于把记忆库挂在公网上
#
# 用法：
#   docker run -v /host/data:/data -v /host/cfg.toml:/etc/zero-memcp/zero-memcp.toml:ro IMAGE
#   docker run IMAGE serve -data /data -config /cfg.toml
set -eu

DATA="${ZERO_MEMCP_DATA:-/data}"
CONF="${ZERO_MEMCP_CONFIG:-/etc/zero-memcp/zero-memcp.toml}"

# 用户显式给了子命令就照办，不塞默认值。
if [ "$#" -gt 0 ]; then
    exec zero-memcp "$@"
fi

if [ ! -f "$CONF" ]; then
    cat >&2 <<'MSG'
zero-memcp: no config file found at /etc/zero-memcp/zero-memcp.toml

The server refuses to bind a non-loopback address from the command line, so a
container cannot be exposed without a config that declares a [[listen]] block:

    [server]
    data         = "/data"
    enforce_auth = true

    [[listen]]
    addr    = "0.0.0.0:1729"
    allow   = ["*"]
    require = "bearer"     # required: a non-loopback bind without credentials
                           # publishes the whole memory store

    [[user]]
    name = "zero"
    root = true

Mount it at /etc/zero-memcp/zero-memcp.toml, or pass your own command:
    docker run ... IMAGE serve -data /data -config /path/to/cfg.toml
MSG
    exit 78
fi

exec zero-memcp serve -data "$DATA" -config "$CONF"
