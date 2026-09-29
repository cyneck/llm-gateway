# ============================================================================
# 编译阶段
# ============================================================================
FROM golang:1.23-alpine AS builder

WORKDIR /src

# 先复制依赖清单（利用 Docker 层缓存；本项目零第三方依赖，实际很快）
COPY go.mod ./
COPY . .

# 静态编译，减小体积
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/llm-gateway ./cmd/llm-gateway

# ============================================================================
# 运行阶段
# ============================================================================
FROM alpine:3.20

# CA 证书：访问 HTTPS 上游必需；tzdata：日志时区
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/llm-gateway /usr/local/bin/llm-gateway

EXPOSE 8318
VOLUME ["/config"]

ENTRYPOINT ["llm-gateway"]
# 容器内监听 0.0.0.0 以便宿主机访问；配置通过挂载 /config 提供
CMD ["-config", "/config/config.json", "-host", "0.0.0.0"]
