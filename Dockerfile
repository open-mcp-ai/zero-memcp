# zero-memcp 官方多架构 OCI 镜像。
#
# 构建：
#   docker build -t ghcr.io/open-mcp-ai/zero-memcp:latest .
#
# 运行：
#   docker run -d \
#     -p 1729:1729 \
#     -v /path/to/data:/data \
#     -v /path/to/config.toml:/etc/zero-memcp/zero-memcp.toml:ro \
#     --name zero-memcp \
#     ghcr.io/open-mcp-ai/zero-memcp:latest
#
# 入口脚本会：数据目录 /data、监听 0.0.0.0:1729、
# 若 /etc/zero-memcp/zero-memcp.toml 存在则自动带上 -config。
# 传自己的参数（如 `... IMAGE serve -data /data -config /cfg.toml`）会覆盖默认。
#
# 容器默认以 uid 1000 (zero) 运行，避免 root 权限在挂载卷上制造无法回收的文件。

FROM golang:1.24-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=$VERSION -X github.com/open-mcp-ai/zero-memcp/internal/server.Version=$VERSION" \
    -o /out/zero-memcp ./cmd/zero-memcp

FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/open-mcp-ai/zero-memcp"
LABEL org.opencontainers.image.description="Git-backed memory server for coding agents (MCP)"
LABEL org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache ca-certificates tzdata git \
    && addgroup -S -g 1000 zero \
    && adduser -S -u 1000 -G zero -h /data zero \
    && mkdir -p /etc/zero-memcp /data \
    && chown -R zero:zero /data

COPY --from=build /out/zero-memcp /usr/local/bin/zero-memcp
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY LICENSE /usr/share/doc/zero-memcp/LICENSE

USER zero
ENV HOME=/data
WORKDIR /data
VOLUME ["/data"]

EXPOSE 1729

# 默认交给入口脚本：serve 不会自己找配置，也不会默认监听 0.0.0.0。
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD []
