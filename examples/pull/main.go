// Command pull 由应用管理 HTTP 服务和退出流程，SDK 只提供采集入口。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	metrics "github.com/zgsm-sangfor/costrict-common-metrics"
)

// 将退出码处理放在最外层，保证 run 中的 defer 先完成资源清理。
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// 1. 应用启动时创建 SDK；SDK 本身不会监听端口或启动周期任务。
	sdk, err := metrics.New(metrics.Config{App: "sdk-example", EmitInstanceName: true})
	if err != nil {
		return err
	}
	// 4. run 返回时再关闭 SDK；正常退出路径会先执行下方 server.Shutdown。
	defer func() {
		// 使用独立的关闭 context，不能使用已经因退出信号而取消的 ctx。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sdk.Shutdown(ctx); err != nil {
			log.Printf("metrics shutdown: %v", err)
		}
	}()
	// 2. 将指标读取入口接到应用路由，每次 HTTP 请求才会触发 SDK 采集。
	mux := http.NewServeMux()
	mux.Handle("/internal/metrics", promhttp.HandlerFor(sdk.Gatherer(), promhttp.HandlerOpts{
		EnableOpenMetrics:   true,            // 按 Accept 头协商 Prometheus 或 OpenMetrics 格式。
		Timeout:             5 * time.Second, // 限制 HTTP 响应等待时间，无法强制取消 Collect。
		MaxRequestsInFlight: 2,               // 限制并发 HTTP 请求数，SDK 内部仍只有一个采集槽位。
		// 默认 HTTPErrorOnError：包含部分采集失败在内的错误会导致 HTTP 错误响应。
	}))
	// 本地默认只监听回环地址；Docker 通过 METRICS_LISTEN_ADDR=:9091
	// 监听容器内所有网卡，让端口映射和 Prometheus 跨容器抓取可达。
	addr := os.Getenv("METRICS_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9091"
	}
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	// 应用自己接管 Ctrl+C / SIGTERM，SDK 不注册系统信号处理器。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 3. HTTP 服务在独立 goroutine 运行，主 goroutine 等待服务错误或退出信号。
	// 缓冲为 1，保证退出时服务 goroutine 能写入结果而不会等待接收方。
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Printf("metrics at http://%s/internal/metrics; identity: %+v", server.Addr, sdk.Identity())
	select {
	case err := <-result:
		// 例如端口被占用时，返回启动错误；正常关闭产生的 ErrServerClosed 不算故障。
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// 收到退出信号后停止接受 HTTP 连接，并等待已进入的请求完成。
		// 此调用返回后，顶部 defer 才执行 SDK 关闭。
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
