# 通用 Go Metrics SDK 设计讨论稿

状态：讨论稿 v3，尚未实现。最新定位：**主要功能是采集指标，再提供接口给接入应用和其他服务读取；内部推送到 Pushgateway 只是可选扩展，不是核心交付前提。** 具体接口与实现契约见 [技术设计文档](./metrics-sdk-technical-design.md)。

SDK 是嵌入式库。同进程直接读取；跨进程由接入应用提供 HTTP/RPC 接口。SDK 不启动 HTTP 服务，支持裸机、Docker、K8s。

## 1. 已确定的职责边界

| 能力 | SDK 负责 | 接入方负责 |
| --- | --- | --- |
| 采集 | Go Runtime、当前进程指标；按配置启用扩展 collector | 选择采集项、配置业务指标 |
| 提供数据 | 返回结构化指标，提供 Prometheus 数据格式编码能力 | 决定何时读取、如何使用 |
| Prometheus 拉取 | 提供可供读取的 Gatherer/指标数据 | 创建 HTTP 服务和路由、处理请求、内容协商、响应、认证及限流 |
| 可选推送 | 提供单次/周期推送接口，内部采集并适配 Pushgateway 协议 | 选择是否启用，提供地址及网络客户端，管理认证和分组清理 |
| 实例身份 | 接收通用身份配置；按策略生成默认 ID；附加标签 | 可覆盖 ID，注入部署元数据，确保自定义 ID 唯一 |
| 目标发现 | 不维护监控目标列表 | 配置 Prometheus 静态目标、服务发现或部署侧注册 |

**SDK 不启动 HTTP Server，不监听端口，不提供绑定请求处理逻辑的 `Handler()`，不接管应用路由和信号。** 接入方可自行使用 `promhttp.HandlerFor(sdk.Gatherer(), ...)`，这是接入方的选择，而非 SDK 内置的接口。

推送是独立可选模块：SDK 提供内部机制，接入方注入 HTTP Client 管理网络传输，不必自行编写 Exporter。未启用时不启动推送任务，不影响任何采集和读取能力。

## 2. 通用架构

```text
Go Runtime / 当前进程 / 自定义 Collector
                   │
          独立 Registry + 身份标签
                   │
          核心：Gather / Gatherer / Encode
                   │
        ┌──────────┴────────────┐
   同进程直接读取         接入应用 HTTP/RPC 接口
                                │
                    其他服务读取 / Prometheus 抓取
                                │
                          Grafana 展示

可选分支：复用 Gather → SDK 内部周期推送 → Pushgateway → Prometheus 抓取
```

核心不读取 K8s API、不访问 Docker Socket、不要求编排平台权限。采用 `github.com/prometheus/client_golang`，优先提供生态兼容的数据接口。推送、Prometheus 和 Grafana 都不是 SDK 采集与读取的前置依赖。

## 3. 多容器的通用解法：标识采集实例

Prometheus 以“指标名称 + 完整标签集合”识别时间序列。需要区分的基本对象是**独立的采集来源，通常就是一个 Go 进程**，而不是某种平台上的容器对象。

一个容器里通常只有一个主业务进程，此时采集实例自然对应容器内的业务副本；一个容器里有多个进程时，每个进程仍需独立身份。不同容器里的 PID 都可能是 1，因此不能仅用 PID 标识实例。

### 3.1 核心标签仅保留两个概念

| 标签 | 含义 | 示例 | 约束 |
| --- | --- | --- | --- |
| `app` | 调用方定义的逻辑应用名 | `billing-api` | 建议必填，不依赖 K8s Service |
| `sdk_instance_id` | 独立采集实例的标识 | 可读前缀 + 随机 UUID，或部署方提供的完整 ID | 同一查询/存储范围内，应保证不同并发来源不碰撞 |

`cluster`、`namespace`、`pod`、`container`、`host`、`region`、`env` 等都是可选扩展标签，没有任何一个是核心运行的前提。

使用 `sdk_instance_id` 而非直接写 `instance`，是为了避免与 Prometheus 默认的抓取目标标签冲突。`job`、`instance` 留给采集端管理；推送协议适配确需设置它们时，应在出口明确约定。

标签名仍是候选设计，但“逻辑应用 + 采集实例”的语义应保持稳定。部署环境之间的应用名若相同，可用 `env` 等可选标签区分聚合范围；实例 ID 的默认生成仍独立保证唯一性。

### 3.2 实例 ID 的优先级和生命周期

已确定采用“可读前缀 + 自动生成的随机 UUID”，按以下优先级确定 ID：

1. 调用方显式设置完整 `InstanceID` 时直接使用，不再自动拼接。
2. 否则确定可读前缀：优先使用 `InstanceName`，其次读取约定环境变量 `METRICS_INSTANCE_NAME`，再尝试操作系统 hostname；空值视为未提供。
3. 使用安全随机源生成 UUID v4。有可用前缀时生成 `<前缀>-<UUID>`，没有时只使用 UUID。随机源失败时返回初始化错误，不能退化为仅使用名称或时间戳。

前缀仅帮助人工定位，随机 UUID 负责不同来源的唯一性。前缀清理规则建议统一为字母、数字、点、下划线和连字符，其他字符替换为下划线，最长保留 128 个字符；清理后为空则省略前缀。前缀即使重复或被截断，也不影响 UUID 的独立性。

示例中的 `<UUID>` 表示完整随机 UUID：

```text
Docker：a1b2c3d4e5f6-<UUID>
K8s：billing-api-7bc89-x2abc-<UUID>
裸机：server01-<UUID>
无可用名称：<UUID>
```

名称获取方式不要求 SDK 识别运行平台：

- K8s 部署方可通过 Downward API 将 `metadata.name` 注入 `METRICS_INSTANCE_NAME`，SDK 只读取通用环境变量。若只需要 Pod 名后缀，由调用方提取并传入 `InstanceName`；SDK 不假定 Pod 命名规则。
- Docker 可显式注入容器名或容器 ID 作为前缀；未注入时使用 hostname。默认 hostname 通常与短容器 ID 有关，但可能被覆盖，不能将其当成已验证的容器 ID。
- 裸机及其他环境同样使用显式名称或 hostname，无需容器元数据。

需要按可读名称查询时，可配置额外输出 `instance_name` 静态标签，取初始化时选定的名称，不从拼接后的 ID 反向解析。此标签属于 SDK 身份配置，启用时不能再由业务指标或自定义 Labels 重复声明。名称只有在部署方保证其稳定时才适合关联重启前后的数据，不能代替唯一实例 ID。

随机 ID 只在初始化时产生，后续每次 Gather、每次推送都保持相同；不能每次采样都重新生成。拉取和推送复用同一个 SDK 实例。推荐一个进程使用一个 SDK；若多个 Registry 表示同一来源，调用方应共享身份，并避免重复导出相同指标。

默认随机 ID 在进程重启后变化，优点是无需平台信息即可隔离来源，降低 IP、名称或端口复用导致历史混淆的风险；代价是重启会产生新序列。容量估算要计入副本数量、重启和滚动发布频率。

如果希望重启前后延续同一逻辑副本的序列，调用方可传入稳定 ID，但需要保证：

- 该 ID 同时只对应一个活动写入者，滚动发布期间也不能重复。
- 持久化 ID 文件不能烘焙进镜像，也不能被多个副本通过共享卷共同复用。
- 默认随机 ID 与显式稳定 ID 的重启语义不同，应在监控配置中明确选择。

SDK 可以验证空值、标签合法性和本地配置冲突，但无法在没有外部协调服务的情况下保证调用方提供的 ID 全局唯一。日志应暴露最终使用的身份，便于排查配置错误。

### 3.3 为什么不单独使用 hostname、IP 或容器 ID 保证唯一性

- hostname 可被显式覆盖，也可能在多个环境中重复；Docker 默认 hostname 的行为不能作为通用唯一性契约。
- IP 会复用，Docker 不同宿主机的私有网络中还可能出现相同 IP；NAT 后地址也不等于原始采集来源。
- 容器内获取完整容器 ID 没有跨运行时、跨 cgroup 版本稳定可靠的通用方式。
- 挂载 Docker Socket 只为读取身份，会引入额外权限与环境依赖。

因此核心 SDK 只把显式名称或 hostname 用作可读前缀，不推断真实容器 ID，不以 IP 或名称保证唯一性。需要准确的平台身份时，由部署方显式注入名称或可选标签。

### 3.4 各种部署环境如何使用

| 环境 | 通用实例身份 | 可选补充信息 | Prometheus 如何找到每个目标 |
| --- | --- | --- | --- |
| 裸机多进程 | 默认随机 ID；或部署方保证唯一的进程槽位 ID | `host`、`env` | 独立端口的静态配置或文件发现 |
| 单机 Docker 多容器 | 每个进程生成独立随机 ID，无需读取容器 ID | 部署方注入容器名/部署名 | 直连各容器端点，或宿主机不同映射端口 |
| 多主机 Docker | 默认随机 ID；不依赖容器私有 IP 唯一 | `host`、`region` | Docker 服务发现、文件发现或可路由的主机映射端口 |
| K8s 多副本 | 仍可使用默认随机 ID | 通过 Downward API/发现规则添加 Pod 信息 | PodMonitor、ServiceMonitor 或 Kubernetes 服务发现 |

平台信息仅用于查询、定位或与外部容器指标关联，不决定核心 SDK 能否运行。若采用 Pod UID 构造显式 ID，还要考虑同 Pod 多容器、多进程；单独一个 Pod UID 不足以区分这些来源。

### 3.5 唯一身份不能替代逐实例发现

无论 Docker 还是 K8s，都不能仅抓一个随机分流到副本的负载均衡地址。即使返回数据带唯一 ID，也可能每次只采到不同副本的一部分，无法连续观察所有实例。

例如三个 Docker 容器分别映射到宿主机 19091、19092、19093 端口，由接入应用提供 `/metrics`：

```yaml
scrape_configs:
  - job_name: billing-api
    static_configs:
      - targets:
          - 'docker-host:19091'
          - 'docker-host:19092'
          - 'docker-host:19093'
```

`docker-host` 应替换为 Prometheus 可访问的实际地址。容器间直连也可以，核心要求是每个端点对应一个可持续识别的来源。动态扩缩容时，目标清单应由平台或服务发现更新，SDK 不承担这项职责。

各容器输出类似：

```text
process_resident_memory_bytes{app="billing-api",sdk_instance_id="<uuid-A>"} ...
process_resident_memory_bytes{app="billing-api",sdk_instance_id="<uuid-B>"} ...
process_resident_memory_bytes{app="billing-api",sdk_instance_id="<uuid-C>"} ...
```

Prometheus 拉取入库时还会附加自身的目标标签。这样既能区分采集实例，又能定位访问端点。默认随机 ID 能解决“数据来自不同实例”的问题；要进一步定位“实例属于哪个具体容器”，需要部署方提供可选元数据或目标发现信息。

## 4. 采集范围及资源口径

| 对象 | 推荐采集方式 | 说明 |
| --- | --- | --- |
| Go Runtime | Go Collector | goroutine、堆、GC、Go 版本等，具体指标随客户端版本确认 |
| 当前进程 | Process Collector | CPU 累计时间、RSS、启动时间、文件描述符等，受操作系统支持限制 |
| 当前进程所属 cgroup | 可选 Linux cgroup Collector | 适用于 Docker/K8s，也可能是裸机 systemd 分组；不能一律称为整个容器 |
| 宿主机 | 独立可选模块或外部 node_exporter | 不把容器视角下的系统信息自动当作宿主机真实资源 |
| 业务指标 | Registerer 扩展入口 | 由调用方注册，SDK 不接管业务 HTTP 流量 |

基础采集不依赖集群监控组件。建议默认启用 Go Runtime 和当前进程指标；如果需求明确包含“整个容器的资源用量”，再将 cgroup 模块纳入首版范围，而不是假定外部一定存在 cAdvisor。

需要特别明确：

- Go 堆内存、进程 RSS、cgroup 内存用量不是同一个数值。
- `rate(process_cpu_seconds_total[5m])` 表示平均消耗的 CPU 核数，不是已经归一化到容器 limit 的百分比。
- cgroup 内存不等同于工作集；若提供 working set，要说明估算公式与平台差异。
- cgroup 采集需适配 v1/v2、命名空间、挂载路径、权限、无上限及配额动态变化；不支持的指标不应伪装为 0。
- 同一 cgroup 内多个进程若都输出完整 cgroup 用量，跨进程求和会重复统计。此类资源指标需要独立的 `resource_id` 与单一采集者约定，不能直接套用进程指标聚合方式。

建议先确认首版的 CPU/内存口径，但保留采集器扩展接口。

### 4.1 默认采集清单：接入后即可观察进程和 Go 运行状态

以下标准指标以最终选定的 Go / `client_golang` 版本为准，不能将表格当作所有版本都具备的接口承诺。Counter 保存累计值，趋势面板一般使用 `rate` / `increase`；Gauge 展示当前值。

| 信息 | 指标示例 | 类型 / 单位 | 最终展示及用途 |
| --- | --- | --- | --- |
| 实例身份 | 所有指标上的 `app`、`sdk_instance_id`；可选 `instance_name` | 标签 | 按应用筛选，区分各副本，定位高负载实例 |
| 进程启动时间 | `process_start_time_seconds` | Gauge，Unix 秒 | 启动时间、运行时长，辅助判断是否发生重启 |
| 进程 CPU 累计消耗 | `process_cpu_seconds_total` | Counter，秒 | CPU 核数趋势、最耗 CPU 的实例排行 |
| 进程常驻内存 | `process_resident_memory_bytes` | Gauge，字节 | RSS 趋势、实例内存排行、进程 RSS 合计 |
| 进程虚拟内存 | `process_virtual_memory_bytes` | Gauge，字节 | 辅助排查地址空间占用，不作为实际物理内存使用量 |
| 已打开文件描述符 / 上限 | `process_open_fds`、`process_max_fds` | Gauge，个 | FD 数量及使用率，排查连接/文件未释放问题 |
| Go 版本 | `go_info{version="..."}` | Gauge，固定值 1 | 实例详情中的 Go 版本，排查版本差异 |
| goroutine 数 | `go_goroutines` | Gauge，个 | 并发变化、持续增长异常；不能仅凭增长断言泄漏 |
| Go Runtime 创建的 OS 线程数 | `go_threads` | Gauge，个 | 线程增长趋势；具体语义依 Go Collector 实现确认 |
| 当前已分配堆内存 | `go_memstats_heap_alloc_bytes` | Gauge，字节 | 活跃堆趋势、GC 后是否持续上涨 |
| 堆已占用 / 空闲 / 已归还内存 | `go_memstats_heap_inuse_bytes`、`go_memstats_heap_idle_bytes`、`go_memstats_heap_released_bytes` | Gauge，字节 | 排查堆分配与 RSS 差异；这些口径有重叠，不能全部相加 |
| Runtime 向系统申请的内存 | `go_memstats_sys_bytes` | Gauge，字节 | 与 Go 堆、RSS 分开对照，不能当作 RSS |
| 活跃堆对象数 | `go_memstats_heap_objects` | Gauge，个 | 对象积累趋势，辅助分析内存增长 |
| 累计分配字节数 | `go_memstats_alloc_bytes_total` | Counter，字节 | 分配速率 B/s，识别分配压力与 GC 压力 |
| 栈内存 | `go_memstats_stack_inuse_bytes` | Gauge，字节 | 配合 goroutine 数观察栈内存增长 |
| 下次 GC 的堆目标 | `go_memstats_next_gc_bytes` | Gauge，字节 | 理解 GC 触发目标，不是容器内存上限 |
| GC 次数及累计暂停时间 | `go_gc_duration_seconds_count`、`go_gc_duration_seconds_sum` | Summary 的 count / sum，次 / 秒 | GC 频率、平均每次暂停、暂停时间速率 |

进程指标在不同操作系统上可用性不同；缺失时面板显示“不可用”，不能补成 0。Go Collector 默认 Summary 暴露的分位数也不能直接跨实例求平均来得到全局 P99；首版 GC 面板优先展示频率和平均暂停。

默认数据能发现资源压力和运行趋势，但不能直接给出泄漏代码位置、goroutine 堆栈或 CPU 热点函数。此类定位需要接入方另行开启 pprof、日志或 tracing，不属于基础 metrics 采集。

### 4.2 SDK 自身状态：确认采集数据是否可信

核心自监控包括版本、最近一次采集完成时间、采集耗时、采集错误次数和忙碌跳过次数。采集结束后补入当次结果，不递归 Gather；标签使用有限枚举，不包含错误全文。

推送状态属于可选模块的本地诊断，不要求基础读取接口或看板依赖它。不设计推送队列和丢弃批次指标；Pushgateway 模式可结合网关 `push_time_seconds` 与 SDK 采集时间判断新鲜度。详细指标名称和类型见技术设计第 5 节。

### 4.3 可选资源采集：需要显式开启与平台支持

| 信息 | 来源 / 候选数据 | 可展示内容 | 限制 |
| --- | --- | --- | --- |
| cgroup CPU 用量与配额 | 使用时间累计值、quota/period 换算的核数 | cgroup CPU 核数、相对配额使用率 | 无配额时百分比显示“不适用”；配额与 cpuset 限制不是同一概念 |
| cgroup CPU 限流 | 被限流周期数、总周期数、限流时间累计值 | 被限流周期比例、限流趋势 | 限流时间不能直接当作业务延迟 |
| cgroup 内存用量与上限 | usage/current、limit/max | 内存趋势、相对 limit 使用率 | 无上限、不可读时明确标记；不能用 Go 堆代替 |
| cgroup OOM 事件 | 平台支持的事件计数 | OOM 事件增量 | 进程被杀前未必有机会发出数据，需外部监控补充 |
| cgroup 块设备 I/O | 平台支持的读写字节/操作累计数 | 读写吞吐、IOPS | cgroup 版本和控制器配置相关，不代表网络 I/O |
| 宿主机 CPU / 内存 / 磁盘 / 网络 | 外部 exporter 或独立主机模块 | 主机总览与资源瓶颈 | 多业务容器不能各自重复上报同一主机资源后相加 |

上述资源指标需另行制定独立命名空间和 `resource_id` 规则。容器网络流量、文件系统容量不由基础 Process Collector 自动提供，不应作为默认承诺。

### 4.4 业务信息：调用方埋点后才能展示

| 调用方提供的指标 | 可展示内容 | 接入要求 |
| --- | --- | --- |
| 请求数，按路由模板、方法、状态分类 | QPS、错误率、接口调用排行 | 调用方自己的 HTTP/RPC 中间件记录 |
| 请求耗时 Histogram | P50/P95/P99 延迟、慢接口趋势 | 调用方埋点，桶边界结合 SLO 配置 |
| 当前处理中请求数 | 并发请求量 | 调用方维护 Gauge |
| 任务成功/失败数、执行耗时、积压量 | 任务吞吐、成功率、积压告警 | 业务逻辑或任务框架提供 |
| 数据库连接池使用量、等待数/耗时 | 连接池压力与等待趋势 | 调用方适配数据库/连接池统计接口 |

这些指标不能只靠引入 SDK 就自动完整获得。SDK 提供注册和输出能力，第三方继续控制业务流量；未接入的模块在看板中隐藏或标记“未启用”。

## 5. 数据接口建议

以下为候选形状，不是可直接编译的现有 API：

```go
sdk, err := metrics.New(metrics.Config{
    App:        "billing-api",
    InstanceID: "", // 非空时直接使用；空值时自动生成
    InstanceName: "", // 可选前缀；空值时依次尝试环境变量、hostname
    Labels:     map[string]string{"env": "prod"}, // 可选
})
// 调用方处理 err。

families, err := sdk.Gather()       // 结构化指标，如 []*dto.MetricFamily
reg := sdk.Registerer()            // 注册业务指标
collector := sdk.Gatherer()        // 供接入方自行适配
identity := sdk.Identity()         // 读取本次实际身份
```

建议核心优先提供 `Gather()`、`Gatherer()`、`Registerer()`；文本编码可作为纯数据辅助函数，例如向调用方提供的 `io.Writer` 写入指标。它只进行编码，不接触 `http.ResponseWriter`，也不决定响应状态、认证或路由。

接入方自己的代码可以使用：

```go
// 以下属于第三方程序，非 SDK 实现。
handler := promhttp.HandlerFor(sdk.Gatherer(), promhttp.HandlerOpts{})
router.Handle("/internal/metrics", handler)
```

需要确定的数据契约：

- `Gather()` 是当前时刻的采集结果；昂贵采集器可以使用有明确更新时间的缓存。
- 不默认给供拉取使用的指标添加显式采样时间戳，由 Prometheus 记录抓取时间；Pushgateway 推送也不添加显式样本时间戳，采集时刻通过普通 Gauge 的值表达。
- 支持并发读取，采集器不能因慢请求无限积压；缓存有效期和错误要可观察。
- 默认独立 Registry，不修改全局默认 Registry，不使用会 panic 的隐式注册路径。
- `app`、`sdk_instance_id` 和自定义静态标签通过统一注册/导出机制应用到所有 SDK 输出；拒绝业务指标重复使用这些保留标签。
- 如后续支持外部 Gatherer 合并，也必须校验身份与标签冲突，不能让某些指标绕过身份约定。
- 明确部分采集失败的返回语义。建议返回可用指标和错误，由接入方决定 HTTP 响应策略；不能静默吞错或填零。
- `Gather()` 返回的数据所有权、是否可修改、可选推送读取的数据隔离规则必须写清楚，避免并发污染。

Prometheus 抓取配置不要再次设置同名 `app`、`sdk_instance_id`，否则可能受 `honor_labels` 影响而被覆盖或改名。平台标签命名冲突应在接入时解决。

## 6. 可选推送：内部机制对接 Pushgateway

SDK 的核心仍是采集并提供读取接口。可选 `pushgateway` 子包提供 `Push`（单次）、`Start`（周期）、`Stop`（停止）和 `Delete`（显式清理）接口，复用核心 Gather。配置及调用方式见技术设计第 6 节。

数据路径：SDK 采集 → 内部推送模块 → Pushgateway 保存快照 → Prometheus 抓取 → Grafana 展示。接入方提供地址、job 和 HTTP Client；SDK 实现周期调度及 Pushgateway 协议适配。remote write 不纳入当前方案。

扩展先采用单任务、请求超时、不积压历史批次的简单机制。单次失败返回错误，周期模式记录结果并在下一周期重新采集；不引入 WAL、队列或复杂重试。部分采集失败不覆盖网关现有完整快照，推送失败也不停止核心读取服务。

分组使用 job、app、sdk_instance_id，禁止共享分组造成覆盖。Pushgateway 不随应用退出自动删除分组，默认随机 ID 重启后会留下旧数据，接入方必须管理清理或过滤过期数据。Stop 不自动 Delete，强杀时退出清理不可靠。

Prometheus 抓取 Pushgateway 使用 `honor_labels: true`。网关 `up=1` 仅表示网关可达，不能证明应用存活；应检查采集时间和推送时间。默认监控链路仍为直接逐实例抓取，不应将相同指标同时经两个路径导入同一查询域。

## 7. 查询和容量建议

不依赖 K8s 标签的查询示例：

```promql
# 每个采集实例的平均 CPU 核数。
sum by (app, sdk_instance_id) (
  rate(process_cpu_seconds_total{app="billing-api"}[5m])
)

# 应用所有已采集进程的 RSS 总量，不是容器总内存。
sum by (app) (
  process_resident_memory_bytes{app="billing-api"}
)

# 按实例观察 goroutine 数。
go_goroutines{app="billing-api"}
```

以上假设同一来源仅导入一次；如果需要按环境区分，可添加可选 `env` 过滤/分组。Counter 应先 `rate` 再聚合。HA Prometheus 汇总到远端时，仍需平台侧配置副本去重，实例 ID 本身不会去除重复采集。

容量按每实例序列数、存活实例数、重启产生的新实例数和保留时间评估。随机实例 ID 的基数与进程生命周期相关，是有意保留的来源身份；不要再附加请求 ID、用户 ID、原始 URL 等无限增长标签。

初始采集/推送周期可考虑 15～30 秒；拉取周期由 Prometheus 管理，推送周期由 SDK 配置，两者不必混用。测试 Gather 耗时、响应数据体积、内存和 CPU 开销。业务延迟分布如使用 Histogram，还需计入桶带来的序列数量。

### 7.1 最终展示由谁提供

Prometheus 负责存储和 PromQL 查询，可使用自身页面检查指标和表达式；完整趋势看板建议由 Grafana 查询 Prometheus 展示。告警可由 Prometheus 规则与 Alertmanager 配合处理。也可以由第三方应用查询 Prometheus 后自行展示。

SDK 不内置网页和看板服务。未来可随 SDK 交付可导入的 Grafana Dashboard JSON、查询及告警规则示例，这些是配套材料，不改变 SDK 的 HTTP 职责边界。下面是展示设计，不是已实现页面。

### 7.2 应用总览：先发现哪个实例异常

顶部筛选：应用 `app`、时间范围、实例；可选增加环境、实例名称等已配置标签。基础页面不要求存在任何 K8s 标签。

示意布局（数值仅用于说明）：

```text
应用：billing-api    时间：最近 1 小时    实例：全部

┌────────────────┬────────────────┬────────────────┬────────────────┐
│ 最近有数据：3  │ CPU：1.24 核   │ 进程 RSS：920MiB│ goroutine：386 │
└────────────────┴────────────────┴────────────────┴────────────────┘

┌────────────────────────────────┬─────────────────────────────────┐
│ 各实例 CPU 核数折线图          │ 各实例 RSS 折线图               │
├────────────────────────────────┼─────────────────────────────────┤
│ Go 堆内存 / 分配速率           │ goroutine 数 / GC 频率          │
└────────────────────────────────┴─────────────────────────────────┘

实例名称      实例 ID        CPU 核数  RSS      goroutine  数据状态
worker-a      ...UUID-A      0.82      420MiB   180        最近有数据
worker-b      ...UUID-B      0.30      310MiB   126        最近有数据
worker-c      ...UUID-C      0.12      190MiB    80        最近有数据
```

这里“最近有数据”不等于“业务健康”，也不代表期望副本数。SDK 不能知道应该运行几个实例；若需要展示“3/4 个副本运行中”，必须由外部配置或部署系统提供期望数量及实例清单。

实例 ID 可在表格中缩短显示，但详情和过滤仍保留完整值。未提供名称时显示 ID。鼠标选择异常实例后进入详情，不能只看应用总量而掩盖单副本偏斜。

### 7.3 实例详情：判断资源压力来自哪里

| 面板区域 | 展示方式 | 能回答的问题 |
| --- | --- | --- |
| 基本信息 | 应用、完整 ID、可选名称、Go/SDK 版本、启动时间、运行时长 | 当前查看哪个进程？运行了多久？版本是否一致？ |
| CPU | 核数折线；有 cgroup 指标时额外展示配额与限流 | 是否持续高负载？是否受到资源配额限制？ |
| 内存 | RSS、Go 堆、Runtime 内存的独立曲线；可选 cgroup 用量/上限 | 内存增长主要在堆内还是堆外？是否接近限制？ |
| Go Runtime | goroutine、线程、对象数、分配速率、栈内存 | 并发和对象分配是否持续增长？ |
| GC | 每秒次数、平均暂停毫秒数、暂停时间速率 | GC 是否越来越频繁或暂停变长？ |
| 进程资源 | FD 数量/比例、虚拟内存 | 是否接近句柄上限？ |
| 采集/推送 | 数据年龄、采集耗时/错误；可选推送时间与本地状态 | 数据异常是业务问题还是采集链路问题？ |

RSS 与堆内存不能作为互斥部分堆叠成总内存；内存上限不存在时不显示虚假的使用百分比。历史范围中的旧实例与当前新实例应分开显示，重启后 UUID 变化并不代表当前同时存在两个副本。

### 7.4 可选页面

- **资源详情**：仅开启 cgroup 或外部资源采集后出现，按资源身份查询，不跨重复采集者求和。
- **业务表现**：仅业务已埋点后展示 QPS、错误率、P95/P99、任务积压和连接池使用情况。
- **推送链路**：仅启用推送时展示推送时间、数据年龄及网关状态；拉取模式展示抓取状态。

### 7.5 典型面板的计算方式

下面表达式演示单实例常用计算，生产看板应统一加入应用和实例过滤；存在多条目标序列时需先确认是否重复采集。

```promql
# 进程运行时长，秒。
time() - process_start_time_seconds{app="billing-api"}

# 进程 RSS，以 MiB 显示；也可直接让 Grafana 转换字节单位。
process_resident_memory_bytes{app="billing-api"} / 1024 / 1024

# Go 内存分配速率，字节/秒。
rate(go_memstats_alloc_bytes_total{app="billing-api"}[5m])

# GC 次数/秒。
rate(go_gc_duration_seconds_count{app="billing-api"}[5m])

# 平均每次 GC 暂停，毫秒；窗口内无 GC 时结果不可用，不伪造为 0。
1000 * rate(go_gc_duration_seconds_sum{app="billing-api"}[5m])
  / rate(go_gc_duration_seconds_count{app="billing-api"}[5m])

# 最近收到的采集批次距离现在的时间，秒。
time() - costrict_sdk_collection_timestamp_seconds{app="billing-api"}
```

数据年龄使用采集端时钟，要求双方时间同步；延迟发送的历史批次会显示更大的年龄，这是需要观察的信号。序列完全消失后，该表达式也不再返回值，需要另设缺失检测。

拉取的 `up` 属于 Prometheus 生成的目标指标，不会自动携带 SDK 指标中的 `app` 和 `sdk_instance_id`。关联实例详情时，应通过实际目标标签（通常包括 `job`、`instance`）连接对应身份信息，或由目标配置提供一致的标签映射。直接推送看板不使用 `up` 判断发送者存活。

### 7.6 告警可以做到什么

建议先提供可调整的规则模板，阈值结合实例配额和业务基线制定：

| 告警方向 | 条件示例 | 必要前提 |
| --- | --- | --- |
| 数据不可达 / 停止更新 | 拉取 `up=0` 持续一段时间；或采集时间超过数个周期未更新 | 完全消失的来源需缺失检测；逐实例缺失告警需外部期望实例清单或明确历史策略 |
| CPU 持续偏高 | 核数超出约定预算；或 cgroup 使用量接近有效配额 | 未知 CPU 配额时不能直接称为“CPU 使用率超过 80%” |
| 内存压力 | RSS 超出应用预算；或 cgroup 内存接近有限上限 | 指标与阈值必须属于同一资源口径 |
| goroutine 异常增长 | 超出基线并持续增长 | 需要结合业务负载判断，不能直接宣称泄漏 |
| FD 接近上限 | 已打开 FD / 有效上限持续偏高 | 平台提供这两个指标且上限大于 0 |
| 可选推送停止更新 | SDK 采集时间或网关推送时间长时间不更新 | 网关仍会暴露旧值；需要新鲜度判断及清理规则 |

告警规则与可视化应区分“0”“无数据”“不支持”“未启用”，避免以绿色 0 值掩盖监控缺失。

## 8. 建议实施顺序与验收

**第一阶段：纯采集与数据能力。** 独立 Registry、Go/Process Collector、通用身份、Gather/编码、业务注册入口。提供第三方自行创建 HTTP 路由的示例，SDK 中不出现监听地址、路由或 HTTP 服务生命周期配置。

配套交付建议包括第 4 节的版本化指标字典，以及第 7 节对应的基础 Grafana 看板和 PromQL 示例；可选采集模块未启用时正确显示不可用状态。看板验收应覆盖多实例筛选、单实例加压、重启后历史与当前实例区分、采集断开及无数据状态。

**第二阶段：采集和读取闭环。** 验证同进程读取、外部服务 HTTP/RPC 读取、Prometheus 逐实例抓取和 Grafana 看板。此阶段不依赖推送模块。

**第三阶段：跨环境与多副本验证。** 至少验证裸机多进程、Docker 多容器，再验证可选 K8s 部署：

1. 无 K8s/Docker API 权限时 SDK 正常运行。
2. 多个 hostname 相同、PID 都为 1 的容器依然产生不同实例 ID。
3. ID 在同一 SDK 生命周期内保持不变，拉取与推送身份一致。
4. 默认 ID 重启后更新，显式 ID 按约定延续；滚动更新不存在并发 ID 冲突。
5. Prometheus 抓取每个端点，单实例加压能够分别观察，再按应用聚合。
6. 部分采集失败、并发读取、慢 collector 和关闭均满足约定；不启用推送时全部核心功能可用。
7. 第三方任意选择路由、端口和 HTTP 框架，SDK 无需修改。
8. 验证完整 ID、显式名称、环境变量、hostname 的优先级，以及名称不可用时只生成 UUID 的回退行为；名称清理不改变 UUID，同名副本仍保持独立身份。

容器资源 collector 是否进入第一阶段，取决于首版资源口径；若纳入，则增加 cgroup v1/v2、无上限、不可读、多进程共享 cgroup 的专项验证。

## 9. 后续扩展事项

在核心采集、数据读取和跨环境验证完成后，再按需实现 Pushgateway 可选模块，并独立验证单次/周期推送、超时、分组隔离、显式删除和重启残留。

cgroup 资源采集仍需独立确认口径、资源身份及重复采集约束，不作为当前默认功能。发布前固定 Go、client_golang 和展示组件版本。

职责优先级为：**先采集，再提供读取接口；推送为可选附加能力。** 外部服务如何访问由接入方决定，Prometheus/Grafana 提供一种标准监控消费方式。
