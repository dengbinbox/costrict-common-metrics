package identity

import (
	"crypto/rand"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Config 提供身份选择所需配置，不负责采集。
type Config struct {
	App              string
	InstanceID       string
	InstanceName     string
	EmitInstanceName bool
	Labels           map[string]string
}

// Identity 是已验证的实例身份及完整静态标签。
type Identity struct {
	App          string
	InstanceID   string
	InstanceName string
	Labels       map[string]string
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ReservedLabel 返回 SDK 或 Prometheus 保留的标签名称。
func ReservedLabel(name string) bool {
	switch name {
	case "app", "sdk_instance_id", "instance_name", "job", "instance":
		return true
	}
	return strings.HasPrefix(name, "__")
}

// New 使用环境变量、主机名和安全随机源生成身份。
func New(cfg Config) (Identity, error) {
	return resolve(cfg, os.Getenv, os.Hostname, rand.Reader)
}

func resolve(cfg Config, getenv func(string) string, hostname func() (string, error), random io.Reader) (Identity, error) {
	if !validValue(cfg.App) || strings.TrimSpace(cfg.App) == "" {
		return Identity{}, fmt.Errorf("app must be nonempty valid UTF-8, at most 256 bytes")
	}
	if !validValue(cfg.InstanceID) || (cfg.InstanceID != "" && strings.TrimSpace(cfg.InstanceID) == "") {
		return Identity{}, fmt.Errorf("invalid instance ID")
	}
	if !validValue(cfg.InstanceName) {
		return Identity{}, fmt.Errorf("invalid instance name")
	}
	if len(cfg.Labels) > 16 {
		return Identity{}, fmt.Errorf("at most 16 custom labels are allowed")
	}
	labels := make(map[string]string, len(cfg.Labels)+3)
	for k, v := range cfg.Labels {
		if !labelName.MatchString(k) || ReservedLabel(k) || !validValue(v) {
			return Identity{}, fmt.Errorf("invalid static label %q", k)
		}
		labels[k] = v
	}
	name := cfg.InstanceName
	if name == "" {
		name = getenv("METRICS_INSTANCE_NAME")
	}
	if name == "" {
		var err error
		name, err = hostname()
		if err != nil {
			name = ""
		}
	}
	if !validValue(name) {
		return Identity{}, fmt.Errorf("invalid selected instance name")
	}
	var clean strings.Builder
	for _, r := range name {
		if clean.Len() == 128 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			clean.WriteRune(r)
		} else {
			clean.WriteByte('_')
		}
	}
	name = clean.String()
	id := cfg.InstanceID
	if id == "" {
		var b [16]byte
		if _, err := io.ReadFull(random, b[:]); err != nil {
			return Identity{}, fmt.Errorf("generate UUID: %w", err)
		}
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		id = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
		if name != "" {
			id = name + "-" + id
		}
	}
	app := strings.TrimSpace(cfg.App)
	labels["app"], labels["sdk_instance_id"] = app, id
	if cfg.EmitInstanceName && name != "" {
		labels["instance_name"] = name
	}
	return Identity{App: app, InstanceID: id, InstanceName: name, Labels: labels}, nil
}

func validValue(v string) bool { return len(v) <= 256 && utf8.ValidString(v) }

// Clone 返回独立的身份副本。
func (i Identity) Clone() Identity { i.Labels = maps.Clone(i.Labels); return i }
