package collection

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

var ErrGatherBusy = errors.New("metrics: gather busy")

// Options 控制默认采集、完整身份标签及 SDK 自监控。
// 零值保留不带标签和自监控的基础采集能力。
type Options struct {
	DisableGo      bool
	DisableProcess bool
	Labels         map[string]string
	Monitor        bool
}

// Collector 管理独立 Registry，必须由 New 创建，不可复制。
type Collector struct {
	registry   *prometheus.Registry
	registerer prometheus.Registerer
	busy       atomic.Bool
	labels     map[string]string
	monitor    bool
	skipped    atomic.Uint64
	failed     [3]atomic.Bool
	errors     [4]uint64
	mu         sync.Mutex
	business   map[string]*checkedCollector
}

var _ prometheus.Gatherer = (*Collector)(nil)

func New(opts Options) (*Collector, error) {
	r := prometheus.NewPedanticRegistry()
	c := &Collector{registry: r, labels: maps.Clone(opts.Labels), monitor: opts.Monitor, business: make(map[string]*checkedCollector)}
	c.registerer = prometheus.WrapRegistererWith(c.labels, r)
	defaults := []struct {
		enabled   bool
		collector prometheus.Collector
		category  int
	}{
		{!opts.DisableGo, collectors.NewGoCollector(), 0},
		{!opts.DisableProcess, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{ReportErrors: true}), 1},
	}
	for _, d := range defaults {
		if !d.enabled {
			continue
		}
		wrapped := &checkedCollector{source: d.collector, failed: &c.failed[d.category]}
		if err := c.registerer.Register(wrapped); err != nil {
			return nil, fmt.Errorf("collection: register default collector: %w", err)
		}
	}
	return c, nil
}

// RecordBusy 记录由上层共用采集入口拒绝的并发请求。
func (c *Collector) RecordBusy() { c.skipped.Add(1) }

// Gather 返回独立快照；部分失败时同时返回有效数据和错误。
func (c *Collector) Gather() ([]*dto.MetricFamily, error) {
	if !c.busy.CompareAndSwap(false, true) {
		c.RecordBusy()
		return nil, ErrGatherBusy
	}
	defer c.busy.Store(false)
	for i := range c.failed {
		c.failed[i].Store(false)
	}
	started := time.Now()
	families, err := c.registry.Gather()
	duration := time.Since(started)
	snapshot := make([]*dto.MetricFamily, 0, len(families)+5)
	for _, family := range families {
		snapshot = append(snapshot, proto.Clone(family).(*dto.MetricFamily))
	}
	if c.monitor {
		classified := false
		for i := range c.failed {
			if c.failed[i].Load() {
				c.errors[i]++
				classified = true
			}
		}
		if err != nil && !classified {
			c.errors[3]++
		}
		completed := time.Now()
		snapshot = append(snapshot, c.selfMetrics(completed, duration)...)
		sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].GetName() < snapshot[j].GetName() })
	}
	return snapshot, err
}
