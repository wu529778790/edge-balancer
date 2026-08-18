# 构建阶段
FROM golang:1.26.4-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o edge-balancer .

# 运行阶段
FROM alpine:3.20
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=builder /app/edge-balancer .
USER app
EXPOSE 8080
ENTRYPOINT ["./edge-balancer", "-config", "/app/config.yaml"]
