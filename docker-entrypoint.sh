#!/bin/sh
# zero-memcp 容器入口。
#
# 存在的理由：`serve` 不会自己去找配置文件，也不会默认监听 0.0.0.0——
# 而在容器里 127.0.0.1 是不可达的。把这两件事写进入口，镜像才「开箱能跑」，
# 同时保留显式覆盖的能力（传自己的参数会覆盖默认值）。
#
# 用法：
#   docker run -v /host/data:/data -v /host/cfg.toml:/etc/zero-memcp/zero-memcp.toml:ro IMAGE
#   docker run IMAGE serve -data /data -config /cfg.toml -listen 0.0.0.0:1729
set -eu

DATA="${ZERO_MEMCP_DATA:-/data}"
CONF="${ZERO_MEMCP_CONFIG:-/etc/zero-memcp/zero-memcp.toml}"
LISTEN="${ZERO_MEMCP_LISTEN:-0.0.0.0:1729}"

# 用户显式给了子命令就照办，不塞默认值。
if [ "$#" -gt 0 ]; then
    exec zero-memcp "$@"
fi

set -- serve -data "$DATA" -listen "$LISTEN"
if [ -f "$CONF" ]; then
    set -- "$@" -config "$CONF"
fi

exec zero-memcp "$@"
