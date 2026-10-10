package collection

import (
	"runtime/debug"
	"sort"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

func buildVersion() string {
	const module = "github.com/zgsm-sangfor/costrict-common-metrics"
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path == module && info.Main.Version != "" {
			return info.Main.Version
		}
		for _, dep := range info.Deps {
			if dep.Path == module {
				if dep.Replace != nil {
					return "(devel)"
				}
				return dep.Version
			}
		}
	}
	return "(devel)"
}

func (c *Collector) selfMetrics(completed time.Time, duration time.Duration) []*dto.MetricFamily {
	gauge := dto.MetricType_GAUGE
	counter := dto.MetricType_COUNTER
	makeFamily := func(name, help string, kind dto.MetricType, value float64, extra map[string]string) *dto.MetricFamily {
		labels := make([]*dto.LabelPair, 0, len(c.labels)+len(extra))
		for k, v := range c.labels {
			labels = append(labels, &dto.LabelPair{Name: proto.String(k), Value: proto.String(v)})
		}
		for k, v := range extra {
			labels = append(labels, &dto.LabelPair{Name: proto.String(k), Value: proto.String(v)})
		}
		sort.Slice(labels, func(i, j int) bool { return labels[i].GetName() < labels[j].GetName() })
		metric := &dto.Metric{Label: labels}
		if kind == gauge {
			metric.Gauge = &dto.Gauge{Value: proto.Float64(value)}
		} else {
			metric.Counter = &dto.Counter{Value: proto.Float64(value)}
		}
		return &dto.MetricFamily{Name: proto.String(name), Help: proto.String(help), Type: &kind, Metric: []*dto.Metric{metric}}
	}
	result := []*dto.MetricFamily{
		makeFamily("costrict_sdk_build_info", "SDK build information.", gauge, 1, map[string]string{"version": buildVersion()}),
		makeFamily("costrict_sdk_collection_timestamp_seconds", "Unix time of the latest completed collection.", gauge, float64(completed.UnixNano())/1e9, nil),
		makeFamily("costrict_sdk_collection_duration_seconds", "Registry collection and validation duration in seconds.", gauge, duration.Seconds(), nil),
		makeFamily("costrict_sdk_collection_skipped_total", "Collections skipped because another collection is running.", counter, float64(c.skipped.Load()), map[string]string{"reason": "busy"}),
	}
	var errorFamily *dto.MetricFamily
	for i, category := range []string{"go", "process", "custom", "registry"} {
		family := makeFamily("costrict_sdk_collection_errors_total", "Collection batches with errors by category.", counter, float64(c.errors[i]), map[string]string{"collector": category})
		if errorFamily == nil {
			errorFamily = family
		} else {
			errorFamily.Metric = append(errorFamily.Metric, family.Metric...)
		}
	}
	return append(result, errorFamily)
}
