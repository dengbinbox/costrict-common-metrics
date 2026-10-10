package identity

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		name, configured, env, host, want string
		hostErr                           error
	}{
		{name: "configured", configured: "worker /一", env: "env", host: "host", want: "worker___"},
		{name: "environment", env: "env", host: "host", want: "env"},
		{name: "hostname", host: "host", want: "host"},
		{name: "no_name", host: "ignored", hostErr: errors.New("unavailable")},
		{name: "truncate", configured: strings.Repeat("a", 200), want: strings.Repeat("a", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := resolve(Config{App: " app ", InstanceName: tc.configured, EmitInstanceName: true}, func(string) string { return tc.env }, func() (string, error) { return tc.host, tc.hostErr }, bytes.NewReader(make([]byte, 16)))
			if err != nil {
				t.Fatal(err)
			}
			if id.App != "app" || id.InstanceName != tc.want {
				t.Fatalf("unexpected identity: %+v", id)
			}
			wantID := "00000000-0000-4000-8000-000000000000"
			if tc.want != "" {
				wantID = tc.want + "-" + wantID
			}
			if id.InstanceID != wantID {
				t.Fatalf("ID = %s", id.InstanceID)
			}
			_, hasName := id.Labels["instance_name"]
			if hasName != (tc.want != "") {
				t.Fatal("incorrect optional name label")
			}
		})
	}
}

func TestExplicitIDAndOwnership(t *testing.T) {
	labels := map[string]string{"env": "prod"}
	id, err := resolve(Config{App: "app", InstanceID: " explicit ", InstanceName: "worker", Labels: labels}, func(string) string { return "" }, func() (string, error) { return "", nil }, brokenReader{})
	if err != nil {
		t.Fatal(err)
	}
	if id.InstanceID != " explicit " {
		t.Fatal("explicit ID was modified")
	}
	if _, ok := id.Labels["instance_name"]; ok {
		t.Fatal("unexpected instance_name")
	}
	labels["env"] = "changed"
	clone := id.Clone()
	clone.Labels["env"] = "other"
	if id.Labels["env"] != "prod" {
		t.Fatal("labels were not copied")
	}
}

func TestInvalidConfig(t *testing.T) {
	cases := []Config{
		{}, {App: " "}, {App: "\xff"}, {App: strings.Repeat("a", 257)},
		{App: "app", InstanceID: " "}, {App: "app", InstanceID: "\xff"},
		{App: "app", InstanceName: "\xff"},
	}
	for _, k := range []string{"app", "sdk_instance_id", "instance_name", "job", "instance", "__reserved", "bad-name", "1name"} {
		cases = append(cases, Config{App: "app", Labels: map[string]string{k: "x"}})
	}
	cases = append(cases, Config{App: "app", Labels: map[string]string{"env": "\xff"}}, Config{App: "app", Labels: map[string]string{"env": strings.Repeat("x", 257)}})
	tooMany := map[string]string{}
	for i := range 17 {
		tooMany[string(rune('a'+i))] = "v"
	}
	cases = append(cases, Config{App: "app", Labels: tooMany})
	for _, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("accepted invalid config: %+v", cfg)
		}
	}
}

func TestRandomFailureAndUUID(t *testing.T) {
	_, err := resolve(Config{App: "app"}, func(string) string { return "" }, func() (string, error) { return "", nil }, brokenReader{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("random failure = %v", err)
	}
	a, err := New(Config{App: "app", InstanceName: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{App: "app", InstanceName: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if a.InstanceID == b.InstanceID {
		t.Fatal("duplicate UUID")
	}
	if !regexp.MustCompile(`^worker-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a.InstanceID) {
		t.Fatal(a.InstanceID)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
