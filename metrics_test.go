package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"
	metrics "github.com/dengbinbox/costrict-common-metrics"
	"google.golang.org/protobuf/proto"
)

func newSDK(t *testing.T) *metrics.SDK {
	t.Helper()
	s, err := metrics.New(metrics.Config{App: "test", InstanceID: "test-1", InstanceName: "worker", Labels: map[string]string{"env": "test"}, DisableGo: true, DisableProcess: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestIdentityAndDefaultMetrics(t *testing.T) {
	labels := map[string]string{"env": "prod"}
	s, err := metrics.New(metrics.Config{App: " app ", InstanceName: "worker", EmitInstanceName: true, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	id := s.Identity()
	labels["env"] = "mutated"
	id.Labels["app"] = "mutated"
	if s.Identity().App != "app" || s.Identity().Labels["env"] != "prod" || s.Identity().Labels["app"] != "app" {
		t.Fatal("identity mutation leaked")
	}
	families, err := s.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range families {
		found[f.GetName()] = true
		for _, m := range f.Metric {
			actual := map[string]string{}
			for _, l := range m.Label {
				if _, exists := actual[l.GetName()]; exists {
					t.Fatal("duplicate label")
				}
				actual[l.GetName()] = l.GetValue()
			}
			for k, v := range s.Identity().Labels {
				if actual[k] != v {
					t.Errorf("%s missing identity %s", f.GetName(), k)
				}
			}
			if m.TimestampMs != nil {
				t.Fatal("explicit timestamp")
			}
		}
	}
	for _, name := range []string{"go_goroutines", "costrict_sdk_build_info", "costrict_sdk_collection_timestamp_seconds", "costrict_sdk_collection_duration_seconds", "costrict_sdk_collection_errors_total", "costrict_sdk_collection_skipped_total"} {
		if !found[name] {
			t.Errorf("missing %s", name)
		}
	}
	if s.Identity().InstanceID != id.InstanceID {
		t.Fatal("unstable ID")
	}
}

func TestConfigurationErrors(t *testing.T) {
	for _, cfg := range []metrics.Config{{}, {App: "app", Labels: map[string]string{"version": "mine"}}, {App: "app", Labels: map[string]string{"reason": "mine"}}, {App: "app", Labels: map[string]string{"collector": "mine"}}} {
		if _, err := metrics.New(cfg); !errors.Is(err, metrics.ErrInvalidConfig) {
			t.Errorf("expected config error, got %v", err)
		}
	}
}

func TestBusinessRegistration(t *testing.T) {
	s := newSDK(t)
	r := s.Registerer()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "requests_total", Help: "Requests."})
	if err := r.Register(counter); err != nil {
		t.Fatal(err)
	}
	counter.Add(3)
	families, err := s.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if family(t, families, "requests_total").Metric[0].Counter.GetValue() != 3 {
		t.Fatal("wrong counter value")
	}
	var duplicate prometheus.AlreadyRegisteredError
	if err := r.Register(counter); !errors.As(err, &duplicate) || duplicate.ExistingCollector != counter || duplicate.NewCollector != counter {
		t.Fatalf("duplicate error: %#v", err)
	}
	if !r.Unregister(counter) || r.Unregister(counter) {
		t.Fatal("incorrect unregister result")
	}
	for _, name := range []string{"app", "sdk_instance_id", "instance_name", "job", "instance", "env", "__private"} {
		for _, constant := range []bool{true, false} {
			var c prometheus.Collector
			if constant {
				c = prometheus.NewGauge(prometheus.GaugeOpts{Name: "bad_labels", Help: "Bad.", ConstLabels: prometheus.Labels{name: "x"}})
			} else {
				c = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bad_labels", Help: "Bad."}, []string{name})
			}
			if err := r.Register(c); err == nil {
				t.Errorf("accepted reserved label %s (constant=%v)", name, constant)
			}
		}
	}
	if err := r.Register(prometheus.NewGauge(prometheus.GaugeOpts{Name: "costrict_sdk_fake", Help: "Bad."})); err == nil {
		t.Fatal("accepted reserved prefix")
	}
	if err := r.Register(&fixture{}); err == nil {
		t.Fatal("accepted unchecked collector")
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("accepted nil collector")
	}
}

func TestDefaultCollectorsProtected(t *testing.T) {
	s, err := metrics.New(metrics.Config{App: "app"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	if s.Registerer().Unregister(collectors.NewGoCollector()) {
		t.Fatal("unregistered default collector")
	}
}

func TestInvalidSamplesAndSelfMonitoring(t *testing.T) {
	for _, kind := range []string{"timestamp", "labels", "prefix", "types", "duplicate", "undeclared"} {
		t.Run(kind, func(t *testing.T) {
			s := newSDK(t)
			good := prometheus.NewDesc("valid_sample", "Valid.", nil, nil)
			bad := prometheus.NewDesc("invalid_sample", "Invalid.", nil, nil)
			f := &fixture{descs: []*prometheus.Desc{good, bad}, collect: func(ch chan<- prometheus.Metric) {
				ch <- prometheus.MustNewConstMetric(good, prometheus.GaugeValue, 7)
				var m prometheus.Metric = prometheus.MustNewConstMetric(bad, prometheus.GaugeValue, 1)
				switch kind {
				case "timestamp":
					m = prometheus.NewMetricWithTimestamp(time.Now(), m)
				case "labels":
					m = alteredMetric{Metric: m, change: func(out *dto.Metric) {
						out.Label = append(out.Label, &dto.LabelPair{Name: proto.String("job"), Value: proto.String("intruder")})
					}}
				case "prefix":
					m = prometheus.MustNewConstMetric(prometheus.NewDesc("costrict_sdk_fake", "Bad.", nil, nil), prometheus.GaugeValue, 1)
				case "types":
					m = alteredMetric{Metric: m, change: func(out *dto.Metric) { out.Counter = &dto.Counter{Value: proto.Float64(1)} }}
				case "duplicate":
					m = prometheus.MustNewConstMetric(good, prometheus.GaugeValue, 9)
				case "undeclared":
					m = prometheus.MustNewConstMetric(prometheus.NewDesc("undeclared_sample", "Bad.", nil, nil), prometheus.GaugeValue, 1)
				}
				ch <- m
			}}
			if err := s.Registerer().Register(f); err != nil {
				t.Fatal(err)
			}
			for attempt := 1; attempt <= 2; attempt++ {
				families, err := s.Gather()
				if err == nil {
					t.Fatal("missing validation error")
				}
				if len(family(t, families, "valid_sample").Metric) != 1 {
					t.Fatal("valid sample missing or duplicate retained")
				}
				for _, f := range families {
					if f.GetName() == "invalid_sample" || f.GetName() == "costrict_sdk_fake" || f.GetName() == "undeclared_sample" {
						t.Fatalf("invalid sample retained: %s", f.GetName())
					}
				}
				sum := 0.0
				for _, m := range family(t, families, "costrict_sdk_collection_errors_total").Metric {
					sum += m.Counter.GetValue()
				}
				if sum != float64(attempt) {
					t.Fatalf("error count = %v", sum)
				}
				if family(t, families, "costrict_sdk_collection_timestamp_seconds").Metric[0].Gauge.GetValue() <= 0 {
					t.Fatal("missing completion timestamp")
				}
			}
		})
	}
}

func TestShutdownAndBusy(t *testing.T) {
	s := newSDK(t)
	r := s.Registerer()
	g := s.Gatherer()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	desc := prometheus.NewDesc("blocked", "Blocked.", nil, nil)
	if err := r.Register(&fixture{descs: []*prometheus.Desc{desc}, collect: func(ch chan<- prometheus.Metric) {
		close(entered)
		<-release
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)
	}}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.Gather(); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("gather did not start")
	}
	if _, err := g.Gather(); !errors.Is(err, metrics.ErrGatherBusy) {
		t.Fatalf("busy error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown = %v", err)
	}
	if _, err := g.Gather(); !errors.Is(err, metrics.ErrClosed) {
		t.Fatal(err)
	}
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "after_shutdown", Help: "Closed."})
	if err := r.Register(gauge); !errors.Is(err, metrics.ErrClosed) {
		t.Fatal(err)
	}
	if r.Unregister(gauge) {
		t.Fatal("unregistered after shutdown")
	}
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gather did not finish")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentOperations(t *testing.T) {
	s := newSDK(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 10 {
				gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: fmt.Sprintf("parallel_%d_%d", i, j), Help: "Concurrent test."})
				if err := s.Registerer().Register(gauge); err != nil {
					t.Error(err)
					return
				}
				gauge.Inc()
				_, err := s.Gather()
				if err != nil && !errors.Is(err, metrics.ErrGatherBusy) {
					t.Error(err)
				}
				s.Identity().Labels["app"] = "modified"
				if !s.Registerer().Unregister(gauge) {
					t.Error("unregister failed")
				}
			}
		}()
	}
	wg.Wait()
}

func family(t *testing.T, fs []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, f := range fs {
		if f.GetName() == name {
			return f
		}
	}
	t.Fatalf("missing family %s", name)
	return nil
}

type fixture struct {
	descs   []*prometheus.Desc
	collect func(chan<- prometheus.Metric)
}

func (f *fixture) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range f.descs {
		ch <- d
	}
}
func (f *fixture) Collect(ch chan<- prometheus.Metric) {
	if f.collect != nil {
		f.collect(ch)
	}
}

type alteredMetric struct {
	prometheus.Metric
	change func(*dto.Metric)
}

func (m alteredMetric) Write(out *dto.Metric) error {
	if err := m.Metric.Write(out); err != nil {
		return err
	}
	m.change(out)
	return nil
}

func TestSDKSnapshotAndBusyCounter(t *testing.T) {
	s := newSDK(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	desc := prometheus.NewDesc("snapshot_sample", "Snapshot.", nil, nil)
	c := &fixture{descs: []*prometheus.Desc{desc}, collect: func(ch chan<- prometheus.Metric) {
		close(entered)
		<-release
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)
	}}
	if err := s.Registerer().Register(c); err != nil {
		t.Fatal(err)
	}
	type gathered struct {
		families []*dto.MetricFamily
		err      error
	}
	done := make(chan gathered, 1)
	go func() { fs, err := s.Gather(); done <- gathered{fs, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("gather did not start")
	}
	if _, err := s.Gatherer().Gather(); !errors.Is(err, metrics.ErrGatherBusy) {
		t.Fatal(err)
	}
	unblock()
	var first gathered
	select {
	case first = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gather did not complete")
	}
	if first.err != nil {
		t.Fatal(first.err)
	}
	skipped := family(t, first.families, "costrict_sdk_collection_skipped_total")
	if skipped.Metric[0].Counter.GetValue() != 1 {
		t.Fatal("busy request was not recorded")
	}
	*skipped.Metric[0].Counter.Value = 123
	*skipped.Metric[0].Label[0].Value = "modified"
	if !s.Registerer().Unregister(c) {
		t.Fatal("unregister failed")
	}
	fs, err := s.Gather()
	if err != nil {
		t.Fatal(err)
	}
	next := family(t, fs, "costrict_sdk_collection_skipped_total")
	if next.Metric[0].Counter.GetValue() != 1 || next.Metric[0].Label[0].GetValue() == "modified" {
		t.Fatal("self-monitoring snapshot mutation leaked")
	}
}

func TestMustRegisterPanics(t *testing.T) {
	s := newSDK(t)
	c := prometheus.NewGauge(prometheus.GaugeOpts{Name: "duplicate_gauge", Help: "Duplicate."})
	s.Registerer().MustRegister(c)
	defer func() {
		if recover() == nil {
			t.Error("MustRegister did not panic")
		}
	}()
	s.Registerer().MustRegister(c)
}
