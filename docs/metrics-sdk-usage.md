# SDK 使用文档

## 1. 适用范围与依赖引入

本 SDK 是嵌入 Go 应用的库，支持 Go Runtime、当前进程和业务指标采集，以及结构化读取、Prometheus/OpenMetrics 文本编码。SDK 使用独立 Registry，不启动 HTTP 服务或周期采样任务。当前未实现推送、容器 cgroup 或宿主机资源采集。

要求 Go 1.25 或更新版本。Module 路径与导入方式为：

```go
import metrics "github.com/dengbinbox/costrict-common-metrics"
```

Go 包名为 `metrics`，与仓库名中的连字符无关。接入方使用根包公开 API，不直接导入 `internal` 包。

### 引入发布版本

在应用的 module 目录中执行以下命令，固定使用 `v0.0.1`。后续升级时显式选择新的版本标签。

```sh
# 下载指定版本。
go get github.com/dengbinbox/costrict-common-metrics@v0.0.1
# 根据应用代码中的 import 整理依赖。
go mod tidy
```

### 本地联调

假设应用与 SDK 目录同级，在应用目录执行：

```sh
# 在应用的 go.mod 中声明 SDK 依赖。
go mod edit -require=github.com/dengbinbox/costrict-common-metrics@v0.0.0
# 将该依赖指向本机 SDK 源码目录，便于修改后直接联调。
go mod edit -replace=github.com/dengbinbox/costrict-common-metrics=../costrict-common-metrics
```

添加下方接入代码后执行 `go mod tidy`。其中 `v0.0.0` 仅为本地 replace 配套的版本占位，不代表已发布版本；SDK 代码从本地目录读取，第三方依赖仍需缓存或下载。路径不同则调整 replace 目标。

切换到远端发布版本时，删除本地替换，再执行上面的 `go get`：

```sh
# 取消本地源码替换，让后续 go get 使用远端版本。
go mod edit -dropreplace=github.com/dengbinbox/costrict-common-metrics
```

## 2. 从初始化到读取：完整示例

接入分为五步：启动时创建 SDK → 启动时注册业务指标 → 业务执行时更新指标 → 需要数据时调用 Gather → 处理或编码快照。Go Runtime 和进程指标由 SDK 自动采集，业务请求数等指标需要应用主动更新。

将以下代码保存为接入应用的 `main.go`，完成依赖配置后执行 `go run .`。该示例采用严格错误策略：采集失败时直接返回错误。

```go
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metrics "github.com/dengbinbox/costrict-common-metrics"
)

func main() {
	// 将主体逻辑放入 run，保证 run 的 defer 清理完成后再由 log.Fatal 退出。
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// 1. 应用启动时创建一个 SDK，后续各业务模块共享它。
	// New 默认启用 Go Runtime 和当前进程采集，但此时不会启动定时采集或 HTTP 服务。
	sdk, err := metrics.New(metrics.Config{
		App:              "billing-api",                   // 必填：所有指标都会带上 app="billing-api"。
		EmitInstanceName: true,                            // 输出 instance_name，方便查看实例可读名称。
		Labels:           map[string]string{"env": "dev"}, // 所有指标统一附加环境标签。
		// 未指定 InstanceID，SDK 自动生成实例 ID，区分同一应用的多个进程。
	})
	if err != nil {
		return err
	}
	// 应用退出时关闭 SDK，最多等待在途采集 5 秒。
	// 在长驻 HTTP 应用中，应先停止 HTTP 服务和其他读取方，再执行这里的关闭。
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sdk.Shutdown(ctx); err != nil {
			log.Printf("metrics shutdown: %v", err)
		}
	}()
	// 记录最终生成的身份，便于将日志中的进程与监控中的实例对应起来。
	log.Printf("metrics identity: %+v", sdk.Identity())

	// 2. 启动时定义并注册业务指标；Counter 表示累计次数，只增不减。
	requests := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "billing_requests_total",      // 指标名称，Counter 通常以 _total 结尾。
		Help: "Completed billing requests.", // 导出到指标文本中的说明。
	})
	// 注册到 SDK 的独立 Registry，才能由 sdk.Gather 导出并自动附加身份标签。
	// 不要在每次请求中重复注册，也不要改用 prometheus 的全局 Register。
	if err := sdk.Registerer().Register(requests); err != nil {
		return err
	}
	// 3. 模拟完成一次业务请求：累计数加 1。
	// 实际接入时，把 Inc 放在业务请求完成的位置，复用启动时创建的 requests。
	requests.Inc()

	// 4. 现在才触发采集，得到 Go、进程、业务和 SDK 自监控的结构化快照。
	families, err := sdk.Gather()
	if err != nil {
		// 本例要求完整成功；接受部分有效数据的处理方式见第 5 节。
		return err
	}
	// 5. 将已有快照转成 Prometheus 文本并写到标准输出。
	// Encode 只做格式转换，不会再次采集，也不会关闭 os.Stdout。
	return metrics.Encode(os.Stdout, families, metrics.PrometheusText)
}
```

建议每个进程创建一个 SDK，业务模块共享其 Registerer 和 Gatherer。输出包含默认指标、`billing_requests_total` 以及 `costrict_sdk_*` 自监控指标，全部附加相同的身份和静态标签。

本例中的 `requests.Inc()` 只执行一次，因此输出的业务计数值为 1。输出形态如下，实例名称和 ID 以实际运行结果为准：

```text
# HELP billing_requests_total Completed billing requests.
# TYPE billing_requests_total counter
billing_requests_total{app="billing-api",env="dev",instance_name="worker-a",sdk_instance_id="worker-a-<UUID>"} 1
```

仓库内也提供 [直接读取示例](../examples/read/main.go) 和 [HTTP 拉取示例](../examples/pull/main.go)：

```sh
# 单次采集并输出指标，然后退出；该示例也演示部分失败时保留有效结果。
go run ./examples/read
# 启动持续运行的 HTTP 示例，每次访问指标端点时采集；按 Ctrl+C 退出。
go run ./examples/pull
```

## 3. 初始化配置与身份

| 字段 | 默认值 | 含义 |
| --- | --- | --- |
| `App` | 空，必须设置 | 应用名称，去除首尾空白后不能为空 |
| `InstanceID` | 自动生成 | 显式提供时原样使用；仅包含空白时无效 |
| `InstanceName` | 环境变量或 hostname | 实例可读名称，参与自动 ID 前缀选择 |
| `EmitInstanceName` | `false` | 为 true 且最终名称非空时附加 `instance_name` 标签 |
| `Labels` | 无自定义标签 | 附加到所有指标的静态标签，例如 `env`、`region` |
| `DisableGo` | `false` | 禁用默认 Go Runtime 采集 |
| `DisableProcess` | `false` | 禁用默认当前进程采集 |

名称选择顺序为非空 `InstanceName` → `METRICS_INSTANCE_NAME` → hostname；选中名称清理到 `[A-Za-z0-9._-]`，其他字符替换为 `_`，最长保留 128 个字符。hostname 获取失败且无其他名称时允许无前缀。

自动 ID 为“名称 + 随机 UUID v4”；没有名称时直接使用 UUID。同一个 SDK 生命周期内 ID 不变，重新创建默认生成新 ID。显式稳定 ID 的唯一性由接入方保证，不能让多个活动实例共用同一个 ID。

`Identity()` 返回 `App`、`InstanceID`、最终 `InstanceName` 和完整标签副本。默认身份标签为 `app` 与 `sdk_instance_id`；`instance_name` 按配置选择性输出。修改输入 Labels 或返回的 Identity.Labels 不会影响 SDK。

配置限制：

- 身份输入及静态标签值必须为有效 UTF-8，单值最长 256 字节。
- 自定义静态标签最多 16 个，名称遵循 `[a-zA-Z_][a-zA-Z0-9_]*`。
- 静态标签不能使用 `__` 前缀、`app`、`sdk_instance_id`、`instance_name`、`job`、`instance`。
- 静态标签还不能使用 `version`、`collector`、`reason`，它们是 SDK 自监控维度。
- 同时关闭 Go 和进程采集后，业务指标和 SDK 自监控仍可读取。

## 4. 注册业务指标

使用 `sdk.Registerer().Register(collector)`，不要用 `prometheus.Register` 或全局默认 Registry 注册希望由 SDK 导出的指标。

以下片段放在已创建 `sdk` 的函数内，需导入 `github.com/prometheus/client_golang/prometheus`：

```go
// 应用启动时定义耗时直方图，按路由模板和 HTTP 方法分别统计。
// Histogram 保留耗时分布，便于后续在 Prometheus 中计算 P95/P99。
latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
    Name:    "billing_request_duration_seconds",
    Help:    "Billing request duration in seconds.",
    // 桶上界的单位是秒：5ms、10ms、50ms、100ms、500ms、1s、5s。
    Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
}, []string{"route", "method"}) // WithLabelValues 的参数必须按这里的顺序传入。
// 只在启动时注册一次；各次请求共享同一个 latency。
if err := sdk.Registerer().Register(latency); err != nil {
    return err
}
// 模拟一次 GET 请求耗时 25ms。真实请求应记录 time.Since(started).Seconds()。
// 使用 /orders/:id 模板，避免每个订单 ID 都生成一组独立指标。
latency.WithLabelValues("/orders/:id", "GET").Observe(0.025)
```

Counter 使用 `Inc`/`Add`，Gauge 使用 `Set`/`Inc`/`Dec`，Histogram 使用 `Observe`。实例身份由 SDK 自动附加，业务指标无需重复声明。路由使用模板，不把订单 ID、用户 ID、原始 URL 或错误全文作为标签。

业务 Collector 不能使用 `costrict_sdk_` 指标前缀、身份及 Prometheus 保留标签，不能声明与 `Config.Labels` 重名的标签。无法通过 Describe 枚举描述符的 unchecked collector 会被拒绝。采集时还会校验实际输出的标签、描述符、类型、重复序列和显式样本时间戳。

重复注册返回错误，可用 `errors.As` 识别 `prometheus.AlreadyRegisteredError`。`MustRegister` 注册失败会 panic，通常优先使用 `Register`。`Unregister(collector)` 返回是否成功注销，仅允许注销业务 Collector，不能注销默认采集项。

自定义 Collector 的 Describe/Collect 必须并发安全且快速返回。有外部 I/O 时，建议由业务侧使用带超时的任务更新缓存，再由 Collect 读取缓存。

## 5. 读取、错误处理与编码

| API | 用途 |
| --- | --- |
| `sdk.Gather()` | 触发采集并返回 `[]*dto.MetricFamily` 和错误 |
| `sdk.Gatherer()` | 返回 `prometheus.Gatherer` 适配器，共用同一采集入口 |
| `metrics.Encode(w, families, format)` | 将已有快照编码为标准指标文本 |
| `metrics.ContentType(format)` | 获取编码格式对应的 Content-Type |

`dto` 对应 `github.com/prometheus/client_model/go`。返回值是独立快照，调用方可以修改，不会影响其他读取或后续采集。SDK 不添加显式样本时间戳；Prometheus 抓取时赋予样本时间。

采集部分失败会同时返回有效指标和非 nil 错误。可以像完整示例一样直接报错，也可以记录错误后继续处理有效部分。以下片段需导入 `errors`、`log` 和 `metrics`，置于返回 error 的函数中：

```go
// 每次调用都会触发新的采集；数据无需等待后台任务预先生成。
families, err := sdk.Gather()
switch {
case errors.Is(err, metrics.ErrClosed):
    // 已进入关闭流程，继续使用这个 SDK 实例也无法恢复读取。
    return err
case errors.Is(err, metrics.ErrGatherBusy):
    // 另一次 Gather 尚未结束；将错误交给调用方，由它决定稍后重试。
    return err
case err != nil:
    // 某些采集项失败，但 families 可能仍有有效数据；本例选择记录错误后继续使用。
    log.Printf("partial metrics: %v", err)
}
// families 是 []*dto.MetricFamily，每个 family 对应一个指标名称。
// 可按下方示例遍历数值，或交给 Encode 输出文本；不要给缺失的指标补零。
```

### 直接读取结构化指标数值

如果调用方只想取得某个指标的数值，可以在上面的错误处理之后遍历 `families`。以下片段需额外导入 `fmt`，与第 2 节注册的计数器配合使用：

```go
for _, family := range families {
    // 一个 family 对应一个指标名，这里只取业务请求累计数。
    if family.GetName() != "billing_requests_total" {
        continue
    }
    // 同名指标可能有多组标签，所以 Metric 是一个切片，不能默认只读取第一个。
    for _, sample := range family.GetMetric() {
        // Counter、Gauge、Histogram 对应不同字段；先确认实际类型。
        if sample.Counter == nil {
            continue
        }
        fmt.Printf("累计请求数：%.0f\n", sample.Counter.GetValue())
        // 标签告诉调用方这个数值属于哪个应用、实例或业务维度。
        for _, label := range sample.GetLabel() {
            fmt.Printf("  %s=%s\n", label.GetName(), label.GetValue())
        }
    }
}
// 没有匹配结果表示本次快照中没有该指标，不能据此认定请求数为 0。
```

公开错误支持 `errors.Is`：

| 错误 | 处理方式 |
| --- | --- |
| `ErrInvalidConfig` | 检查配置和包装错误中的原因，修正后重新初始化 |
| `ErrGatherBusy` | 已有采集在进行，稍后按调用方策略重试 |
| `ErrClosed` | SDK 已关闭，停止使用该实例采集或注册 |
| 其他采集错误 | 检查 Collector、平台支持及错误详情，决定是否接受部分结果 |

文本格式包括 `PrometheusText`（`text/plain; version=0.0.4; charset=utf-8`）和 `OpenMetricsText`（`application/openmetrics-text; version=1.0.0; charset=utf-8`）。后者自动写出 EOF 标记。`Encode` 不再次采集、不修改输入、不关闭 Writer；格式无效或写入失败会返回错误。

编码失败可能已输出部分内容。自定义 HTTP 接口若要求完整成功后再响应，应先编码到 `bytes.Buffer`，成功后设置 `ContentType` 并写入响应。JSON/RPC 接入由应用自行定义 DTO 和部分失败字段，不建议直接将 protobuf Go 结构作为长期 REST 契约。

## 6. HTTP / Prometheus 接入

将 SDK 挂到应用已有路由，不需要为 SDK 单独启动服务。以下片段需导入 `net/http`、`time` 和 `github.com/prometheus/client_golang/prometheus/promhttp`：

```go
// 应用启动时配置路由；已有 mux 的项目直接复用，不必再创建一个。
mux := http.NewServeMux()
mux.Handle("/internal/metrics", promhttp.HandlerFor(
    // Handler 每次收到请求时通过这个适配器调用 SDK 采集，无需自己再调用 Gather。
    sdk.Gatherer(),
    promhttp.HandlerOpts{
        EnableOpenMetrics:   true,                      // 根据 Accept 头协商 Prometheus 或 OpenMetrics。
        ErrorHandling:       promhttp.HTTPErrorOnError, // 部分采集失败也返回 HTTP 错误。
        MaxRequestsInFlight: 2,                         // 限制同时处理的 HTTP 请求数。
        Timeout:             5 * time.Second,           // HTTP 响应超时；不能强制取消阻塞的 Collect。
    },
))
// 将 mux 接到应用管理的 http.Server；完整启动与退出流程见 examples/pull。
```

`promhttp` 负责内容协商。此处部分采集失败也会导致 HTTP 错误响应；如选择 `ContinueOnError`，应配合日志和采集错误监控，HTTP 成功不再表示所有 Collector 成功。`MaxRequestsInFlight` 控制 HTTP 请求数，SDK 自身仍只有一个采集槽位。

本地拉取示例监听 `127.0.0.1:9091`：

```sh
# 在另一个终端读取 Prometheus 文本；每次请求都会触发一次采集。
curl http://127.0.0.1:9091/internal/metrics
# 显式请求 OpenMetrics 格式，响应末尾会包含 # EOF。
curl -H 'Accept: application/openmetrics-text; version=1.0.0' http://127.0.0.1:9091/internal/metrics
```

同机运行 Prometheus 时，可使用以下抓取配置：

```yaml
scrape_configs:
  - job_name: billing-api           # Prometheus 中的抓取任务名，不替代 SDK 的 app 标签。
    metrics_path: /internal/metrics # 必须与应用注册的路由一致。
    scrape_interval: 15s            # 每 15 秒发起一次 HTTP 抓取。
    scrape_timeout: 5s              # 单次抓取最多等待 5 秒。
    static_configs:
      # 地址必须从 Prometheus 所在环境可达，每个目标对应一个应用实例。
      - targets: ['127.0.0.1:9091']
```

Prometheus 在容器或其他机器上时，目标地址应改为其可达的应用地址，应用监听地址也需相应调整。每个目标应稳定对应一个实例，不要只抓负载均衡入口。抓取配置不要再次定义 SDK 已输出的身份或静态标签。指标端点的认证和访问控制由应用负责。

## 7. 关闭与并发

退出时先停止业务触发的新读取及 HTTP 服务，再关闭 SDK。以下片段需导入 `context` 和 `time`：

```go
// 此时应已停止 HTTP 服务以及业务侧发起的新读取。
// 使用新的关闭超时，避免复用已经因退出信号而取消的 context。
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel() // 释放超时计时器。
// 禁止新采集和注册，等待已经开始的采集完成。
if err := sdk.Shutdown(ctx); err != nil {
    // 超时不代表强制停止了 Collect；SDK 仍保持关闭状态。
    return err
}
```

Shutdown 幂等，会禁止新采集和注册，并等待在途采集。超时返回 context 错误，SDK 仍保持关闭；之后可以再次调用 Shutdown 等待原采集完成。关闭后，已持有的 Gatherer/Registerer 同样受限制：读取和注册返回 `ErrClosed`，注销返回 false。已持有的业务 Counter 可继续更新，但不再导出。

标准 Prometheus Collector 不支持 context。HTTP 超时和 Shutdown 都不能强行中止阻塞的 Collect；一个卡住的 Collector 会持续占用采集槽位。SDK 不会为等待者启动新的采集 goroutine。

## 8. 默认指标与自监控

默认 Go 指标包括 goroutine、堆内存、GC 等；进程指标包括 CPU 累计时间、RSS、启动时间等，具体可用项依赖操作系统和上游库版本。CPU 核数应通过 `rate(process_cpu_seconds_total[5m])` 等计算，不能在没有资源配额时直接标为 CPU 百分比。

| SDK 指标 | 含义 |
| --- | --- |
| `costrict_sdk_build_info{version}` | 版本信息，值固定为 1；本地开发构建通常为 `(devel)` |
| `costrict_sdk_collection_timestamp_seconds` | 最近一次完成采集的 Unix 秒，部分成功也更新 |
| `costrict_sdk_collection_duration_seconds` | Registry 采集及校验耗时，不含编码和网络传输 |
| `costrict_sdk_collection_errors_total{collector}` | 错误批次数，类别为 `go`、`process`、`custom`、`registry` |
| `costrict_sdk_collection_skipped_total{reason="busy"}` | 因采集槽位忙而拒绝的次数 |

每批每个错误类别最多加 1，错误全文不会作为标签。自监控随正常 Gather 返回，不依赖后台更新；完全没有读取时不会自行刷新。更多架构、指标语义与监控方案见 [技术设计](metrics-sdk-technical-design.md)。

## Docker 运行与 Prometheus 验证

已有 HTTP 拉取示例可构建为容器，使用独立宿主机端口 `19091`，接入现有 Prometheus。启动、重载配置及查询步骤见 [Docker 接入验证文档](metrics-sdk-docker.md)。
