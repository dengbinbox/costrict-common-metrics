package metrics

import (
	"fmt"
	"io"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"google.golang.org/protobuf/proto"
)

// TextFormat 指定输出文本格式。
type TextFormat uint8

const (
	PrometheusText TextFormat = iota
	OpenMetricsText
)

// ContentType 返回对应格式的 HTTP Content-Type。
func ContentType(format TextFormat) (string, error) {
	switch format {
	case PrometheusText:
		return string(expfmt.FmtText), nil
	case OpenMetricsText:
		return string(expfmt.FmtOpenMetrics_1_0_0), nil
	default:
		return "", fmt.Errorf("metrics: unknown text format %d", format)
	}
}

// Encode 编码已有快照，不再次采集，不修改输入，也不关闭 Writer。
// 写入错误可能发生在部分数据已经输出之后。
func Encode(w io.Writer, families []*dto.MetricFamily, format TextFormat) error {
	contentType, err := ContentType(format)
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("metrics: nil writer")
	}
	encoder := expfmt.NewEncoder(w, expfmt.Format(contentType))
	for _, f := range families {
		if f == nil {
			return fmt.Errorf("metrics: nil metric family")
		}
		if err := encoder.Encode(proto.Clone(f).(*dto.MetricFamily)); err != nil {
			return err
		}
	}
	return encoder.(expfmt.Closer).Close()
}
