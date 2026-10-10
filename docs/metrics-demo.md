# Prometheus + Grafana 指标演示

此环境用于预览 Metrics SDK 能展示哪些数据，**原三实例看板的 Go/进程/业务及配额指标由 Python 合成；另有真实容器看板直接读取 cgroup v2**。默认模拟三个实例 `worker-a/b/c`，其中 worker-a 负载更高，曲线每秒变化。无需安装 Python 三方依赖，不启用推送或 Pushgateway。

## 启动与访问

在仓库根目录执行：

```bash
mkdir -p data/prometheus data/grafana data/simulator
LOCAL_UID=$(id -u) LOCAL_GID=$(id -g) docker compose -f docker-compose-base.yaml up -d
```

容器默认使用 UID/GID 1000。上面的环境变量使容器以当前用户写入 bind mount；目录必须由该用户拥有。Compose 固定项目名 `costrict-metrics-demo`，不会覆盖其他项目的同名服务。宿主机 8888、9090、3000 端口需要空闲。

| 入口 | 地址 | 用途 |
| --- | --- | --- |
| Grafana 看板 | http://localhost:3000/d/metrics-sdk-demo | 自动配置的数据源和 29 个展示面板 |
| Prometheus | http://localhost:9090 | 执行 PromQL、检查 Targets |
| 全部模拟指标 | http://localhost:8888/metrics | 文本查看全部三实例数据 |
| 单实例指标 | http://localhost:8888/metrics/worker-a | worker-b/c 同理 |
| 存活检查 | http://localhost:8888/health | 模拟程序 HTTP 状态 |

Grafana 默认账号 `admin`，密码 `metrics-demo-admin`。首次启动前可通过 `GRAFANA_ADMIN_PASSWORD` 覆盖；已初始化的 Grafana 数据库不会因为修改该变量而自动重置密码。远程访问时将 localhost 换为运行 Docker 的机器地址。

Prometheus 每 5 秒分别抓取三个单实例端点；不是负载均衡，也不同时抓取 `/metrics`，避免重复统计。Grafana 每 5 秒刷新，默认显示最近 15 分钟，支持应用与实例筛选。启动约 20～30 秒后 rate/QPS/GC/P95 面板有足够样本，持续运行数分钟可看到完整趋势。

## 可以看到哪些数据

| 类别 | 看板内容 | 指标示例及口径 |
| --- | --- | --- |
| 总览 | CPU、RSS、Goroutine 总量、可见实例数 | 可按实例筛选，不表示真实部署健康 |
| CPU | 每实例 CPU 核数 | `rate(process_cpu_seconds_total[1m])`，不是配额百分比 |
| 内存 | RSS、Go 活跃堆、堆分配速率 | `process_resident_memory_bytes`、`go_memstats_heap_alloc_bytes`、`rate(go_memstats_alloc_bytes_total[1m])` |
| 并发 | Goroutine、OS 线程 | `go_goroutines`、`go_threads` |
| GC | 每秒 GC 次数、平均暂停 | Summary 的 count/sum；不是全局 P99 |
| 进程 | FD 使用比例、模拟进程运行时长 | `process_open_fds/process_max_fds`、启动时间 |
| 采集链路 | 采集数据年龄、耗时、错误及忙碌跳过速率 | 错误和跳过默认是 0，属于正常模拟状态 |
| 业务 | QPS、5xx 错误率、P95、路由 QPS | 模拟 `/ping`、`/orders`、`/reports`，经典 Histogram |

原始 `/metrics` 还输出虚拟内存、堆 in-use/idle/released、Runtime sys、堆对象、栈、下次 GC 目标和 Go/SDK 版本，可在 Prometheus 临时查询。RSS、Go 堆和 Runtime 内存口径重叠，不能相加；本环境额外模拟独立的 cgroup 用量与配额，用于展示资源占比，仍不读取真实宿主机或容器的限制。

模拟器后台每秒更新累计 Counter，抓取不会增加请求数。Histogram 从同一组模拟延迟观测生成，bucket 累计、+Inf 与 count 一致。为便于连续观察，模拟 ID、累计值、模拟启动时间每 5 秒保存到 `state.json`，正常退出也保存；重启恢复同一条虚拟时间线。这与真实 SDK 默认重启生成新 UUID 的语义不同。异常强杀可能丢失最近 5 秒状态，Prometheus rate 按计数器重置处理。

## 文件与数据

```text
docker-compose-base.yaml
compose/prometheus/prometheus.yml
compose/grafana/provisioning/          # 数据源和看板自动配置
compose/grafana/dashboards/metrics-sdk-demo.json
compose/simulator/metrics_simulator.py

data/prometheus/                      # TSDB、WAL，保留 7 天
data/grafana/                         # SQLite、插件及日志目录
data/simulator/state.json             # 模拟累计值快照
```

所有应用持久化数据均 bind mount 到 `./data`，配置文件以只读方式挂载自 `./compose`。没有顶层 Docker volumes 定义，也不使用命名卷或匿名数据卷；Docker 自身的容器元数据和 stdout 日志仍由 Docker 管理。`data/` 已被 Git 忽略。

## 常用操作

```bash
# 状态与日志
docker compose -f docker-compose-base.yaml ps
docker compose -f docker-compose-base.yaml logs --tail=100

# 校验 Prometheus 配置
docker compose -f docker-compose-base.yaml exec prometheus promtool check config /etc/prometheus/prometheus.yml

# 停止：保留 data 中的历史数据
docker compose -f docker-compose-base.yaml down
```

单独在宿主机运行模拟器（避免与已启动容器争用 8888）：

```bash
python3 compose/simulator/metrics_simulator.py --port 8888 --data-dir data/simulator
```

Prometheus 验证查询：

```promql
up{job="metrics-sdk-demo"}
count(costrict_sdk_build_info{app="python-metrics-example"})
sum by (sdk_instance_id) (rate(process_cpu_seconds_total{app="python-metrics-example"}[1m]))
histogram_quantile(0.95, sum by (le, sdk_instance_id) (rate(billing_http_request_duration_seconds_bucket[1m])))
```

前两项应分别返回三个值为 1 的目标、实例数 3。Grafana 无数据时先检查这两项，再检查数据源 URL 为 `http://prometheus:9090`。停止模拟器可验证 `up=0` 和无数据行为；恢复后继续采集。不要将本演示的合成数值当作 SDK 性能测试结果。

## CPU / 内存资源占比

看板总览下方增加 cgroup CPU 配额使用率、内存上限使用率两张趋势图，以及配额值和是否限额的状态卡。

| 模拟实例 | CPU 配额 | 内存上限 | 展示行为 |
| --- | --- | --- | --- |
| worker-a | 1.5 核（1500m） | 512 MiB | 两种使用率随负载变化 |
| worker-b | 0.5 核（500m） | 384 MiB | 相同绝对用量会占更高比例 |
| worker-c | 未配置 | 未配置 | 状态显示未配置，占比不绘制，不伪装为 0% |

例如 CPU 平均使用 0.4 核，配额 0.5 核，使用率为 80%；cgroup 内存使用 256 MiB，上限 512 MiB，使用率为 50%。原来的进程 CPU/RSS 面板仍保留，分别观察进程与资源组口径。

新增指标使用独立 `resource_id`，每个模拟 cgroup 只有一个采集者：

- `costrict_cgroup_cpu_usage_seconds_total`：整个 cgroup 的 CPU 累计秒数。
- `costrict_cgroup_cpu_quota_cores`：有限 CPU 配额核数；无限额时不输出。
- `costrict_cgroup_memory_usage_bytes`：整个 cgroup 内存用量，含缓存等，不是 working set。
- `costrict_cgroup_memory_limit_bytes`：有限内存上限；无限额时不输出。
- `costrict_cgroup_cpu_limit_configured` / `costrict_cgroup_memory_limit_configured`：1=有上限，0=确认无上限，缺失=未知或不支持。

看板计算（实际面板还包含应用/实例过滤）：

```promql
rate(costrict_cgroup_cpu_usage_seconds_total[1m])
/ (costrict_cgroup_cpu_quota_cores > 0)

costrict_cgroup_memory_usage_bytes
/ (costrict_cgroup_memory_limit_bytes > 0)
```

Grafana 使用 `percentunit`，将 0.8 显示成 80%，不能再重复乘 100。80% 和 90% 辅助线仅供观察，不是已配置的告警。CPU 窗口平均值除以当前配额，在配额刚变化时可能暂时超过 100%，不应强行截断。

真实 K8s / Docker 接入时应增加 cgroup collector：v2 读取 `cpu.stat`、`cpu.max`、`memory.current`、`memory.max`；v1 对应 `cpuacct.usage`、`cpu.cfs_quota_us/cpu.cfs_period_us`、`memory.usage_in_bytes/memory.limit_in_bytes`。quota/period 得到 CPU 核数，v1 CPU 纳秒需转换为秒。K8s requests 是调度请求量，不是限制，不能用作本面板分母；Docker CPU shares 也不是硬配额。

实际有效约束还可能来自祖先 cgroup、cpuset 和宿主机可用资源。collector 必须解析当前进程的 cgroup 与挂载路径，并明确展示的是本级配额还是可见层级有效上限；不能只检查固定路径或把不可见的祖先约束当作不存在。内存 v1 的无限额哨兵和 v2 的 max 要识别为无限额。配额可以动态变化，应每次采样更新。不支持、权限不足、无上限必须区分。多个进程共享 cgroup 时不能将相同资源用量重复求和；也不能简单平均多个资源的百分比。

也可直接复用集群已有的 cAdvisor 等资源指标，按容器/资源身份关联，而不由每个业务进程重复采集。三实例页面验证模拟展示；新增的真实容器页面验证 Python 读取当前 cgroup v2。真实 Go SDK 的跨平台资源 collector 仍未实现。


## 从 Compose 限制读取真实 CPU / 内存数据

访问 [真实容器看板](http://localhost:3000/d/metrics-container-real)，或原模拟看板顶部的“真实容器”链接。原三实例数字仍为合成值；真实指标独立暴露在 `http://localhost:8888/metrics/real`，Prometheus 使用独立 job `metrics-container-real`，避免重复汇总。

例如在 `docker-compose-base.yaml` 对 simulator 设置（下列是示例，当前值以实际配置和内核读数为准）：

```yaml
services:
  simulator:
    cpus: "0.50"
    mem_limit: 256m
```

普通 `docker compose up` 即可生效，无需 Swarm。修改后执行 `docker compose -f docker-compose-base.yaml up -d simulator` 重建容器；仅 restart 不会应用 Compose 资源变更。

数据路径是 Compose → Docker 为容器设置 cgroup → Python 从 `/proc/self/cgroup` 和 `/proc/self/mountinfo` 定位 cgroup v2 目录 → 读取资源文件。Python 不读取 Compose、不挂载 Docker Socket，也不要求特权模式。

| 内核文件 | 示例值 / 含义 | 导出指标 |
| --- | --- | --- |
| `cpu.max` | `50000 100000`，50000/100000 = 0.5 核 | `costrict_cgroup_cpu_quota_cores` |
| `memory.max` | `268435456` 字节 = 256 MiB | `costrict_cgroup_memory_limit_bytes` |
| `cpu.stat` 中的 `usage_usec` | 真实累计 CPU 微秒，除以 1e6 转秒 | `costrict_cgroup_cpu_usage_seconds_total` |
| `memory.current` | 当前 cgroup 真实内存字节数，包含缓存等 | `costrict_cgroup_memory_usage_bytes` |

真实面板 CPU 使用率为累计 CPU 秒数的 rate / 实时读取的 CPU quota；内存使用率为 memory.current / 实时读取的 memory.max。分母不是写死的示例常量。真实数据附加 `source="real-cgroup"`、`app="metrics-simulator"`，同一个真实容器只采集一次，不能分别当作三个虚拟 worker 的实际用量。运行负载较低时真实占比会明显低于模拟曲线。

实现文件：`compose/simulator/cgroup_metrics.py`。本演示实现 **cgroup v2 本级限制**，`limit_scope="local"`；未实现 v1 和隐藏祖先/cpuset 的有效限制推导。有限额返回数值，无限额省略分母并输出 configured=0；文件不可读/格式错误时省略该源的指标并增加 `costrict_cgroup_read_errors` 当前错误源数量，不伪装成零用量或无限额。

验证命令：

```bash
docker compose -f docker-compose-base.yaml exec simulator cat /sys/fs/cgroup/cpu.max
docker compose -f docker-compose-base.yaml exec simulator cat /sys/fs/cgroup/memory.max
curl http://localhost:8888/metrics/real
```

容器重建后真实资源 ID 随 hostname 变化，CPU 计数器也会重置；rate 会处理重置，但仍需等待几个抓取周期产生新曲线。cgroup 内存限制可能触发 OOM，CPU 限额可能导致限流，这里配置限制是为验证真实读取，并非改变模拟 worker 的设定值。

排查限制未更新时，先确认打开 `/d/metrics-container-real` 而非模拟看板。查询 `/metrics/real` 和 Prometheus 的 `costrict_cgroup_cpu_quota_cores{source="real-cgroup"}`，并保持 Grafana 时间范围结束于 now。历史时间范围仍会展示当时的旧配额，这是正常的历史数据。
