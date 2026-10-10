package metrics_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	metrics "github.com/dengbinbox/costrict-common-metrics"
	"google.golang.org/protobuf/proto"
)

func TestEncode(t *testing.T) {
	s := newSDK(t)
	histogram := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "request_duration_seconds", Help: "Request duration.", Buckets: []float64{.1, .5, 1}})
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "requests_total", Help: "Requests."})
	s.Registerer().MustRegister(histogram, counter)
	histogram.Observe(.3)
	counter.Inc()
	families, err := s.Gather()
	if err != nil {
		t.Fatal(err)
	}
	original := make([]*dto.MetricFamily, len(families))
	for i, f := range families {
		original[i] = proto.Clone(f).(*dto.MetricFamily)
	}
	for _, format := range []metrics.TextFormat{metrics.PrometheusText, metrics.OpenMetricsText} {
		w := &ownedWriter{}
		if err := metrics.Encode(w, families, format); err != nil {
			t.Fatal(err)
		}
		if w.closed {
			t.Fatal("writer was closed")
		}
		if strings.HasSuffix(w.String(), "# EOF\n") != (format == metrics.OpenMetricsText) {
			t.Fatal("incorrect EOF marker")
		}
		contentType, err := metrics.ContentType(format)
		if err != nil {
			t.Fatal(err)
		}
		if format == metrics.OpenMetricsText && contentType != "application/openmetrics-text; version=1.0.0; charset=utf-8" {
			t.Fatal(contentType)
		}
		if format == metrics.PrometheusText {
			if contentType != "text/plain; version=0.0.4; charset=utf-8" {
				t.Fatal(contentType)
			}
			parser := expfmt.NewTextParser(model.LegacyValidation)
			parsed, err := parser.TextToMetricFamilies(bytes.NewReader(w.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if parsed["requests_total"].Metric[0].Counter.GetValue() != 1 {
				t.Fatal("counter did not round trip")
			}
			if parsed["request_duration_seconds"].Metric[0].Histogram.GetSampleCount() != 1 {
				t.Fatal("histogram did not round trip")
			}
		}
		for i, f := range families {
			if !proto.Equal(f, original[i]) {
				t.Fatal("encoding mutated input")
			}
		}
	}
	var empty bytes.Buffer
	if err := metrics.Encode(&empty, nil, metrics.OpenMetricsText); err != nil || empty.String() != "# EOF\n" {
		t.Fatalf("empty OpenMetrics: %q, %v", empty.String(), err)
	}
}

func TestEncodeErrors(t *testing.T) {
	if _, err := metrics.ContentType(99); err == nil {
		t.Fatal("accepted unknown format")
	}
	var buf bytes.Buffer
	if err := metrics.Encode(&buf, nil, 99); err == nil || buf.Len() != 0 {
		t.Fatal("unknown format wrote output")
	}
	if err := metrics.Encode(nil, nil, metrics.PrometheusText); err == nil {
		t.Fatal("accepted nil writer")
	}
	if err := metrics.Encode(&buf, []*dto.MetricFamily{nil}, metrics.PrometheusText); err == nil {
		t.Fatal("accepted nil family")
	}
	s := newSDK(t)
	families, err := s.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []metrics.TextFormat{metrics.PrometheusText, metrics.OpenMetricsText} {
		if err := metrics.Encode(failingWriter{}, families, format); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("writer error = %v", err)
		}
	}
	if err := metrics.Encode(failingWriter{}, nil, metrics.OpenMetricsText); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("EOF write error = %v", err)
	}
}

func TestHTTPGathererIntegration(t *testing.T) {
	s := newSDK(t)
	handler := promhttp.HandlerFor(s.Gatherer(), promhttp.HandlerOpts{EnableOpenMetrics: true})
	for _, accept := range []string{"text/plain; version=0.0.4", "application/openmetrics-text; version=1.0.0"} {
		request := httptest.NewRequest(http.MethodGet, "/internal/metrics", nil)
		request.Header.Set("Accept", accept)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `app="test"`) {
			t.Fatalf("HTTP response: %d %s", response.Code, response.Body.String())
		}
		if !strings.HasPrefix(response.Header().Get("Content-Type"), strings.Split(accept, ";")[0]) {
			t.Fatal("content negotiation failed")
		}
	}
}

type ownedWriter struct {
	bytes.Buffer
	closed bool
}

func (w *ownedWriter) Close() error { w.closed = true; return nil }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
