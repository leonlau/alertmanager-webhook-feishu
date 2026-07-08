# syntax=docker/dockerfile:1.6

# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src

# 先复制 go.mod / go.sum 单独成层，利用 Docker 缓存
COPY go.mod go.sum ./
RUN go mod download

# 复制源码
COPY . .

# 静态链接、剥离调试信息，缩小最终镜像
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o /out/alertmanager-webhook-feishu .

# ---- runtime stage ----
FROM alpine:3.20
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/alertmanager-webhook-feishu /app/alertmanager-webhook-feishu

# 以非 root 用户运行（飞书 webhook 不需要特权端口）
RUN addgroup -S app && adduser -S app -G app \
    && chown -R app:app /app
USER app

EXPOSE 8000
ENTRYPOINT ["/app/alertmanager-webhook-feishu"]