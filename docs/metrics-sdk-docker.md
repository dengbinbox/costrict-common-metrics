# 使用 Docker 验证真实 SDK 采集

项目有两个示例：`examples/read` 单次读取并退出，`examples/pull` 持续提供 HTTP 指标端点。本方案将后者编译成 Docker 镜像，接入现有 Prometheus，采集真实 Go Runtime、当前进程和 SDK 自监控指标。

## 文件与访问地址

| 文件 | 作用 |
| --- | --- |
| `examples/pull/Dockerfile` | 多阶段构建静态 Go 程序，最终镜像使用 scratch，以非 root 用户运行 |
| `docker-compose-sdk.yaml` | 独立 Compose 项目 `costrict-metrics-sdk`，管理 SDK 示例容器 |
| `compose/prometheus/prometheus.yml` | 新增 `metrics-sdk-real` 抓取任务，保留现有模拟器任务 |
| `docker-compose-base.yaml` | 原有 Prometheus、Grafana、模拟器服务 |

| 入口 | 地址 |
| --- | --- |
| SDK 指标原文 | http://localhost:19091/internal/metrics |
| Prometheus Targets | http://localhost:9090/targets |
| Prometheus 查询 | http://localhost:9090/query |
| Grafana | http://localhost:3000 |

宿主机 `19091` 映射到容器 `9091`。Prometheus 与 SDK 共享 `costrict-metrics-demo_default` 网络，通过 `http://sdk-example:9091/internal/metrics` 抓取；不要在 Prometheus 容器里用 `localhost:19091`。

## 启动与配置重载

在项目根目录执行。若 base 栈已运行，可跳过第一步：

```sh
# 1. 首次启动基础监控栈；目录由当前用户创建，供容器写入数据。
mkdir -p data/prometheus data/grafana data/simulator
LOCAL_UID=$(id -u) LOCAL_GID=$(id -g) docker compose -f docker-compose-base.yaml up -d

# 2. 构建并启动真实 SDK 示例；需要已有 base 栈创建的共享网络。
# Docker 构建阶段会下载 Go 依赖，构建环境需能访问 Go 模块代理。
docker compose -f docker-compose-sdk.yaml up -d --build

# 3. 检查 Prometheus 配置，校验成功后再重载。
docker compose -f docker-compose-base.yaml exec -T prometheus \
  promtool check config /etc/prometheus/prometheus.yml

# 4. 向 Prometheus 发送 SIGHUP，重新读取挂载的配置，无需重启或清空数据。
docker compose -f docker-compose-base.yaml kill -s SIGHUP prometheus
```

如果默认 Go 模块代理超时，可通过构建参数选择当前网络可达的代理，例如：

```sh
# 仅影响镜像构建时下载 Go 依赖；运行中的 SDK 不依赖此代理。
GOPROXY=https://goproxy.cn,direct docker compose -f docker-compose-sdk.yaml up -d --build
```

示例通过 `METRICS_LISTEN_ADDR=:9091` 监听容器所有网卡；本机直接 `go run ./examples/pull` 时默认仍为 `127.0.0.1:9091`。`METRICS_INSTANCE_NAME=sdk-docker-example` 提供可读身份前缀，最终 `sdk_instance_id` 还包含随机 UUID。

若宿主机 19091 已占用，调整 Compose 的端口映射左侧即可；容器内地址和 Prometheus 配置无需改动。独立 Compose 依赖 base 栈网络，不能在 base 栈尚未创建网络时单独启动。

## 检查真实数据

```sh
# 查看容器状态与启动日志，确认实际生成的实例身份。
docker compose -f docker-compose-sdk.yaml ps
docker compose -f docker-compose-sdk.yaml logs --tail=30 sdk-example

# 直接读取示例指标，应该看到 go_*、process_* 和 costrict_sdk_*。
curl -fsS http://localhost:19091/internal/metrics

# 默认每 5 秒抓取一次；启动后等待至少一个抓取周期再查询。
# value 中最后一项为 "1"，表示 Prometheus 成功抓取目标。
curl -fsSG http://localhost:9090/api/v1/query \
  --data-urlencode 'query=up{job="metrics-sdk-real"}'

# 查询真实 Go 进程的 goroutine 数。
curl -fsSG http://localhost:9090/api/v1/query \
  --data-urlencode 'query=go_goroutines{app="sdk-example"}'
```

也可在 Prometheus 查询页执行以下 PromQL：

```promql
# 抓取状态，正常应为 1。
up{job="metrics-sdk-real"}

# Go goroutine 数量。
go_goroutines{app="sdk-example"}

# 当前进程 RSS，单位字节；不是整个容器的内存用量。
process_resident_memory_bytes{app="sdk-example"}

# CPU 使用核数，至少采集两个样本后才可能得到 rate 结果。
rate(process_cpu_seconds_total{app="sdk-example"}[1m])

# 采集数据年龄，单位秒。
time() - costrict_sdk_collection_timestamp_seconds{app="sdk-example"}
```

真实 SDK 使用 `job="metrics-sdk-real"`、`app="sdk-example"`，与模拟数据分开。进程资源会随实际运行变化，当前 HTTP 示例没有业务请求埋点，也没有 cgroup CPU/内存配额指标；不能将其看作模拟器的三实例负载数据。

## 停止与排查

```sh
# 仅停止 SDK 示例；不会停止现有 Prometheus、Grafana 或模拟器。
docker compose -f docker-compose-sdk.yaml down
```

停止后 Prometheus 中该目标显示 DOWN，这是保留抓取配置的正常结果。若不再使用此示例，删除 `metrics-sdk-real` 抓取任务，再校验配置并发送 SIGHUP。

- Targets 中提示 DNS 解析失败：检查 SDK 容器是否已加入共享网络并启动。
- 提示 connection refused：确认 `METRICS_LISTEN_ADDR` 为 `:9091`，不能仅监听容器回环地址。
- 宿主机 curl 成功但 Targets 没有新任务：检查 Prometheus 是否成功重载配置。
- 没有 CPU rate 数据：先等待积累多个样本，并确认进程指标存在。
