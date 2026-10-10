// Package metrics 提供进程内指标采集、业务指标注册和快照编码。
// SDK 不管理 HTTP 服务，不启动周期任务或网络连接。
package metrics

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/zgsm-sangfor/costrict-common-metrics/internal/collection"
	"github.com/zgsm-sangfor/costrict-common-metrics/internal/identity"
)

var (
	ErrInvalidConfig = errors.New("metrics: invalid config")
	ErrClosed        = errors.New("metrics: closed")
	ErrGatherBusy    = collection.ErrGatherBusy
)

// Config 控制身份、静态标签和默认采集项。
type Config struct {
	App              string
	InstanceID       string
	InstanceName     string
	EmitInstanceName bool
	Labels           map[string]string
	DisableGo        bool
	DisableProcess   bool
}

// Identity 描述最终采用的身份，Labels 包含完整静态标签。
type Identity struct {
	App          string
	InstanceID   string
	InstanceName string
	Labels       map[string]string
}

// SDK 必须通过 New 创建，不可复制。方法可并发调用。
type SDK struct {
	identity  identity.Identity
	collector *collection.Collector
	mu        sync.Mutex
	closed    bool
	active    chan struct{}
}

// New 初始化独立采集器；不启动后台任务。
func New(cfg Config) (*SDK, error) {
	for _, name := range []string{"version", "collector", "reason"} {
		if _, exists := cfg.Labels[name]; exists {
			return nil, fmt.Errorf("%w: static label %q conflicts with SDK self-monitoring labels", ErrInvalidConfig, name)
		}
	}
	id, err := identity.New(identity.Config{App: cfg.App, InstanceID: cfg.InstanceID, InstanceName: cfg.InstanceName, EmitInstanceName: cfg.EmitInstanceName, Labels: cfg.Labels})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	c, err := collection.New(collection.Options{DisableGo: cfg.DisableGo, DisableProcess: cfg.DisableProcess, Labels: id.Labels, Monitor: true})
	if err != nil {
		return nil, err
	}
	return &SDK{identity: id, collector: c}, nil
}

// Identity 返回独立副本，调用方修改标签不会影响 SDK。
func (s *SDK) Identity() Identity {
	id := s.identity.Clone()
	return Identity{App: id.App, InstanceID: id.InstanceID, InstanceName: id.InstanceName, Labels: id.Labels}
}

// Registerer 返回受生命周期和身份约束的业务注册入口。
func (s *SDK) Registerer() prometheus.Registerer { return registerer{s} }

// Gatherer 返回与 Gather 共用的采集入口。
func (s *SDK) Gatherer() prometheus.Gatherer { return prometheus.GathererFunc(s.Gather) }

// Gather 返回独立快照；部分失败时仍返回有效数据。
func (s *SDK) Gather() ([]*dto.MetricFamily, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if s.active != nil {
		s.mu.Unlock()
		s.collector.RecordBusy()
		return nil, ErrGatherBusy
	}
	done := make(chan struct{})
	s.active = done
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active = nil; close(done); s.mu.Unlock() }()
	return s.collector.Gather()
}

// Shutdown 阻止新采集和注册，并等待在途采集。超时后仍保持关闭状态。
// 标准 Collector 不支持取消；本方法不能强制中止阻塞的 Collect。
func (s *SDK) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	done := s.active
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type registerer struct{ s *SDK }

func (r registerer) Register(c prometheus.Collector) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.closed {
		return ErrClosed
	}
	return r.s.collector.Register(c)
}
func (r registerer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}
func (r registerer) Unregister(c prometheus.Collector) bool {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.closed {
		return false
	}
	return r.s.collector.Unregister(c)
}
