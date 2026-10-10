# 在构建阶段编译实际 SDK 示例；依赖单独复制以复用构建缓存。
FROM golang:1.25.8 AS builder
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/metrics-pull ./examples/pull

# 静态 Go 程序不需要运行时镜像或 shell。
FROM scratch
COPY --from=builder /out/metrics-pull /metrics-pull
USER 65532:65532
ENV METRICS_LISTEN_ADDR=:9091
EXPOSE 9091
ENTRYPOINT ["/metrics-pull"]
