package collection

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestDefaultCollectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"defaults", Options{}},
		{"without_go", Options{DisableGo: true}},
		{"without_process", Options{DisableProcess: true}},
		{"empty", Options{DisableGo: true, DisableProcess: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCollector(t, tc.opts)
			families, err := c.Gather()
			if err != nil {
				t.Fatal(err)
			}
			var hasGo, hasProcess bool
			for _, f := range families {
				hasGo = hasGo || strings.HasPrefix(f.GetName(), "go_")
				hasProcess = hasProcess || strings.HasPrefix(f.GetName(), "process_")
				for _, m := range f.Metric {
					if m.TimestampMs != nil {
						t.Errorf("%s has an explicit timestamp", f.GetName())
					}
				}
			}
			if hasGo != !tc.opts.DisableGo {
				t.Errorf("Go metrics present = %v", hasGo)
			}
			// 进程指标依赖平台；Linux 下必须存在。
			if tc.opts.DisableProcess && hasProcess || runtime.GOOS == "linux" && !tc.opts.DisableProcess && !hasProcess {
				t.Errorf("process metrics present = %v", hasProcess)
			}
		})
	}
}

func TestRegistryIsolation(t *testing.T) {
	a := newCollector(t, Options{DisableGo: true, DisableProcess: true})
	b := newCollector(t, Options{DisableGo: true, DisableProcess: true})
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "isolated_value", Help: "Test value."})
	if err := a.registry.Register(gauge); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Collector{a, b} {
		families, err := c.Gather()
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if c == a {
			want = 1
		}
		if len(families) != want {
			t.Fatalf("got %d families, want %d", len(families), want)
		}
	}
}

func TestSnapshotOwnership(t *testing.T) {
	c := newCollector(t, Options{DisableGo: true, DisableProcess: true})
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "snapshot_value", Help: "Test snapshot.", ConstLabels: prometheus.Labels{"source": "original"},
	})
	gauge.Set(42)
	if err := c.registry.Register(gauge); err != nil {
		t.Fatal(err)
	}
	first, err := c.Gather()
	if err != nil {
		t.Fatal(err)
	}
	*first[0].Name = "modified"
	*first[0].Metric[0].Label[0].Value = "modified"
	*first[0].Metric[0].Gauge.Value = -1
	second, err := c.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if second[0].GetName() != "snapshot_value" || second[0].Metric[0].Label[0].GetValue() != "original" || second[0].Metric[0].Gauge.GetValue() != 42 {
		t.Fatalf("snapshot mutation leaked: %v", second)
	}
}

func TestPartialFailure(t *testing.T) {
	c := newCollector(t, Options{DisableGo: true, DisableProcess: true})
	desc := prometheus.NewDesc("test_value", "Test value.", nil, nil)
	failure := errors.New("sample unavailable")
	stub := &testCollector{desc: desc, collect: func(ch chan<- prometheus.Metric) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 7)
		ch <- prometheus.NewInvalidMetric(desc, failure)
	}}
	if err := c.registry.Register(stub); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		families, err := c.Gather()
		if err == nil || !strings.Contains(err.Error(), failure.Error()) {
			t.Fatalf("expected collector error, got %v", err)
		}
		if len(families) != 1 || len(families[0].Metric) != 1 || families[0].Metric[0].Gauge.GetValue() != 7 {
			t.Fatalf("valid sample lost or invalid sample retained: %v", families)
		}
	}
}

func TestConcurrentGather(t *testing.T) {
	c := newCollector(t, Options{DisableGo: true, DisableProcess: true})
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	desc := prometheus.NewDesc("blocking_value", "Test blocking collector.", nil, nil)
	stub := &testCollector{desc: desc, collect: func(ch chan<- prometheus.Metric) {
		entered <- struct{}{}
		<-release
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)
	}}
	if err := c.registry.Register(stub); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Gather(); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not start")
	}
	busy := make(chan error, 1)
	go func() { _, err := c.Gather(); busy <- err }()
	select {
	case err := <-busy:
		if !errors.Is(err, ErrGatherBusy) {
			t.Fatalf("expected busy error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Gather blocked")
	}
	release <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Gather did not finish")
	}
	if c.busy.Load() {
		t.Fatal("gather slot was not released")
	}
}

func newCollector(t *testing.T, opts Options) *Collector {
	t.Helper()
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type testCollector struct {
	desc    *prometheus.Desc
	collect func(chan<- prometheus.Metric)
}

func (c *testCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c *testCollector) Collect(ch chan<- prometheus.Metric) { c.collect(ch) }
