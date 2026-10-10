# Costrict Common Metrics

Go 进程内指标 SDK，依据 [技术设计](docs/metrics-sdk-technical-design.md) 实现核心采集与读取。默认采集 Go Runtime 和当前进程，支持业务 Collector、统一身份标签、独立快照、文本编码及自监控。SDK 不启动 HTTP 服务、后台周期任务或网络连接。

当前不包含 Pushgateway 推送、cgroup/宿主机采集或部署看板。Go 版本为 1.25，使用 `prometheus/client_golang` v1.23.2。Module 路径为 `github.com/zgsm-sangfor/costrict-common-metrics`，Go 包名为 `metrics`。

## 使用

完整接入步骤、配置项、错误处理及关闭流程见 [SDK 使用文档](docs/metrics-sdk-usage.md)。

```go
// 应用启动时创建一次 SDK，默认采集 Go Runtime 和当前进程。
sdk, err := metrics.New(metrics.Config{
    App:              "billing-api",
    EmitInstanceName: true,
    Labels:           map[string]string{"env": "prod"},
})
if err != nil {
    return err
}
// 应用退出时执行；HTTP 应用应先停止服务和其他读取方，再关闭 SDK。
defer sdk.Shutdown(context.Background())

// 启动时定义业务累计计数器，表示已完成的请求总数。
requests := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "billing_requests_total",
    Help: "Completed billing requests.",
})
// 注册到 SDK，导出时自动附加 app、sdk_instance_id 和 env 标签。
if err := sdk.Registerer().Register(requests); err != nil {
    return err
}
// 模拟完成一次请求；实际项目将此操作放在请求完成的位置。
requests.Inc()

// 按需采集，返回独立的结构化快照，不需要先启动后台任务。
families, gatherErr := sdk.Gather()
if gatherErr != nil {
    // 部分失败仍可能返回有效数据，是否继续编码由接入方决定。
    log.Printf("metrics collection: %v", gatherErr)
}
// 将已有快照编码到调用方提供的 Writer；不会再次采集，也不会关闭 w。
return metrics.Encode(w, families, metrics.PrometheusText)
```

上例需导入 `context`、`log`、`metrics "github.com/zgsm-sangfor/costrict-common-metrics"` 和 `github.com/prometheus/client_golang/prometheus`；`w` 为调用方管理的 `io.Writer`。可运行的完整示例：

```sh
go run ./examples/read
go run ./examples/pull
# pull 示例启动后，在另一个终端读取：
curl http://127.0.0.1:9091/internal/metrics
```

HTTP 接入使用 `promhttp.HandlerFor(sdk.Gatherer(), ...)`，由应用管理路由、服务和退出流程。直接编码可选 `PrometheusText` 或 `OpenMetricsText`；`ContentType` 返回相应媒体类型，OpenMetrics 输出包含 EOF，`Encode` 不关闭 Writer。

## 运行契约

- `App` 必填；自动实例 ID 使用清理后的名称加安全随机 UUID v4。名称优先级为 `InstanceName`、`METRICS_INSTANCE_NAME`、hostname。显式 `InstanceID` 原样保留。
- `Identity()` 返回完整身份副本。配置标签及读取快照均独立复制，调用方修改不会影响后续采集。
- `DisableGo`、`DisableProcess` 分别关闭默认采集项。进程指标取决于平台支持，不代表容器或宿主机资源。
- 自定义标签最多 16 个，值必须为有效 UTF-8 且不超过 256 字节。标签名遵循传统 Prometheus 规则，不能使用 `__` 前缀或 `app`、`sdk_instance_id`、`instance_name`、`job`、`instance`；静态标签还不能占用自监控维度 `version`、`collector`、`reason`。
- 业务 Collector 不能使用 `costrict_sdk_` 指标前缀、保留标签或与静态标签重名的维度；拒绝 unchecked collector。注册阶段只调用 Describe，采集阶段再次校验描述符、类型、标签、重复序列和显式时间戳。
- `Gather` 和 `Gatherer` 共用单采集入口；并发请求立即返回 `ErrGatherBusy`，部分失败返回有效指标和非 nil 错误，不用零值代替失败数据。
- `Shutdown(ctx)` 幂等，禁止新采集和注册，等待在途采集。超时返回 context 错误且保持关闭状态。关闭后读取和注册返回 `ErrClosed`，注销返回 false；已持有的业务 Counter 仍可更新，但不再导出。
- Collector 的 Describe/Collect 必须并发安全且快速返回。标准 Collector 不支持 context，HTTP 超时或 Shutdown 无法强制取消阻塞的采集。

自监控包含版本、采集完成时间、采集耗时、分类别错误批次数和忙请求次数。版本从 Go 构建信息读取，本地未发布构建显示 `(devel)`。默认 Collector 可观察到的错误按 `go`/`process` 归类，业务样本校验错误按 `custom` 归类，无法归因的 Registry 错误按 `registry` 归类。

## 包结构与验证

```text
metrics.go                 # Config / SDK / Identity / 生命周期与公开错误
encode.go                  # Prometheus / OpenMetrics 文本编码
internal/identity/         # 身份校验、名称选择、UUID
internal/collection/       # Registry、注册及样本校验、快照与自监控
pushgateway/               # 占位，尚未实现
examples/read/             # 同进程采集与编码
examples/pull/             # 应用管理 HTTP 读取端点
examples/pushgateway/      # 占位
```

```sh
go test -race ./...
go vet ./...
go build ./...
```

测试覆盖身份规则、默认采集、注册约束、动态无效样本、快照所有权、并发与关闭、编码及 HTTP 内容协商。底层 Prometheus 描述符未公开名称 getter，保留指标前缀检查依赖固定版本的 `Desc.String()` 表示，升级依赖时需运行对应回归测试。

## Docker 运行与 Prometheus 验证

已有 HTTP 拉取示例可构建为容器，使用独立宿主机端口 `19091`，接入现有 Prometheus。启动、重载配置及查询步骤见 [Docker 接入验证文档](docs/metrics-sdk-docker.md)。
