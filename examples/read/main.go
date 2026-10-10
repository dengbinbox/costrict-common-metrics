// Command read 展示同进程读取和文本编码。
package main

import (
	"context"
	"log"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	metrics "github.com/zgsm-sangfor/costrict-common-metrics"
)

// run 返回后 defer 已执行，再由 main 决定是否以错误状态退出。
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// 1. 启动时初始化一次，默认启用 Go Runtime 和当前进程采集。
	// 自动生成实例 ID，EmitInstanceName 让输出额外包含可读的实例名称。
	sdk, err := metrics.New(metrics.Config{App: "read-example", EmitInstanceName: true})
	if err != nil {
		return err
	}
	// 本例同步执行一次采集后退出，此时没有在途采集需要等待。
	defer sdk.Shutdown(context.Background())
	// 2. 定义业务累计计数器，并注册到 SDK 的独立 Registry。
	// 注册只需在启动时做一次，后续复用同一个 requests。
	requests := prometheus.NewCounter(prometheus.CounterOpts{Name: "example_requests_total", Help: "Example requests."})
	if err := sdk.Registerer().Register(requests); err != nil {
		return err
	}
	// 3. 模拟完成一次业务请求；实际业务应在请求完成的位置累加。
	requests.Inc()
	// 将最终身份写入日志，方便与导出的 app/sdk_instance_id 标签对应。
	log.Printf("metrics identity: %+v", sdk.Identity())
	// 4. 触发一次采集，返回按指标名称分组的结构化快照。
	families, gatherErr := sdk.Gather()
	if gatherErr != nil {
		// 本例接受部分结果：记录错误后仍输出有效指标，不为缺失指标补零。
		log.Printf("partial metrics: %v", gatherErr)
	}
	// 5. 仅将已有快照编码为 Prometheus 文本，不会再次采集或关闭 stdout。
	if err := metrics.Encode(os.Stdout, families, metrics.PrometheusText); err != nil {
		return err
	}
	// 即使有效部分已经输出，也将采集错误返回，让调用者知道结果不完整。
	return gatherErr
}
