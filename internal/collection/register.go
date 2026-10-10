package collection

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/dengbinbox/costrict-common-metrics/internal/identity"
)

// Register 校验描述符并注册业务 Collector，不接受 unchecked collector。
func (c *Collector) Register(source prometheus.Collector) error {
	if source == nil {
		return fmt.Errorf("collection: nil collector")
	}
	descs, key, err := describe(source)
	if err != nil {
		return err
	}
	for _, d := range descs {
		// client_golang 未提供描述符名称 getter；此检查只读取固定版本的 String 前缀。
		if reservedMetric(d) {
			return fmt.Errorf("collection: reserved metric prefix costrict_sdk_")
		}
	}
	wrapped := &checkedCollector{source: source, descs: descs, custom: true, labels: c.labels, failed: &c.failed[2]}
	// 给描述符附加所有受保护标签，让上游检查常量/变量标签冲突。
	// 只调用 Describe，注册阶段绝不触发 Collect。
	forbidden := prometheus.Labels{"app": "", "sdk_instance_id": "", "instance_name": "", "job": "", "instance": ""}
	for k := range c.labels {
		forbidden[k] = ""
	}
	probe := prometheus.WrapRegistererWith(forbidden, prometheus.NewRegistry())
	if err := probe.Register(wrapped); err != nil {
		return fmt.Errorf("collection: invalid collector descriptor: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.registerer.Register(wrapped); err != nil {
		if duplicate, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := duplicate.ExistingCollector.(*checkedCollector); ok {
				duplicate.ExistingCollector = existing.source
			}
			duplicate.NewCollector = source
			return duplicate
		}
		return err
	}
	c.business[key] = wrapped
	return nil
}

// Unregister 仅允许注销已注册的业务 Collector。
func (c *Collector) Unregister(source prometheus.Collector) bool {
	if source == nil {
		return false
	}
	_, key, err := describe(source)
	if err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	registered, ok := c.business[key]
	if !ok {
		return false
	}
	if !c.registerer.Unregister(registered) {
		return false
	}
	delete(c.business, key)
	return true
}

func describe(source prometheus.Collector) ([]*prometheus.Desc, string, error) {
	ch := make(chan *prometheus.Desc)
	go func() { defer close(ch); source.Describe(ch) }()
	var descs []*prometheus.Desc
	keys := make(map[string]struct{})
	var err error
	for d := range ch {
		if d == nil {
			err = fmt.Errorf("collection: nil descriptor")
			continue
		}
		descs = append(descs, d)
		keys[d.String()] = struct{}{}
	}
	if err != nil {
		return nil, "", err
	}
	if len(descs) == 0 {
		return nil, "", fmt.Errorf("collection: unchecked collectors are not supported")
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	return descs, strings.Join(ordered, "\n"), nil
}

func reservedMetric(d *prometheus.Desc) bool {
	return strings.HasPrefix(d.String(), `Desc{fqName: "costrict_sdk_`)
}

type checkedCollector struct {
	source prometheus.Collector
	descs  []*prometheus.Desc
	custom bool
	labels map[string]string
	failed *atomic.Bool
}

func (c *checkedCollector) Describe(ch chan<- *prometheus.Desc) {
	if c.descs == nil {
		c.source.Describe(ch)
		return
	}
	for _, d := range c.descs {
		ch <- d
	}
}
func (c *checkedCollector) Collect(ch chan<- prometheus.Metric) {
	metrics := make(chan prometheus.Metric)
	go func() { defer close(metrics); c.source.Collect(metrics) }()
	for m := range metrics {
		if m == nil {
			c.failed.Store(true)
			ch <- prometheus.NewInvalidMetric(prometheus.NewInvalidDesc(fmt.Errorf("nil metric")), fmt.Errorf("collection: nil metric"))
			continue
		}
		ch <- checkedMetric{Metric: m, owner: c}
	}
}

type checkedMetric struct {
	prometheus.Metric
	owner *checkedCollector
}

func (m checkedMetric) Write(out *dto.Metric) error {
	err := m.Metric.Write(out)
	if err == nil {
		err = m.validate(out)
	}
	if err != nil {
		m.owner.failed.Store(true)
	}
	return err
}
func (m checkedMetric) validate(out *dto.Metric) error {
	types := 0
	for _, present := range []bool{out.Gauge != nil, out.Counter != nil, out.Summary != nil, out.Histogram != nil, out.Untyped != nil} {
		if present {
			types++
		}
	}
	if types != 1 {
		return fmt.Errorf("collection: metric must contain exactly one value type")
	}
	if out.TimestampMs != nil {
		return fmt.Errorf("collection: explicit sample timestamps are not allowed")
	}
	if !m.owner.custom {
		return nil
	}
	if reservedMetric(m.Desc()) {
		return fmt.Errorf("collection: reserved metric prefix costrict_sdk_")
	}
	for _, pair := range out.Label {
		name := pair.GetName()
		_, conflict := m.owner.labels[name]
		if identity.ReservedLabel(name) || conflict {
			return fmt.Errorf("collection: reserved label %q", name)
		}
	}
	return nil
}
