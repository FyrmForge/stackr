package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogCap(t *testing.T) {
	pools := `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}],"firewall-backend":"iptables"}`
	for _, c := range []struct {
		name, file string // file "" = absent
		restarts   int
		warn       bool
		want       map[string]string // log-opts after; nil = file unchanged
	}{
		{name: "absent", restarts: 1, want: logOpts},
		{name: "unrelated keys", file: pools, restarts: 1, want: logOpts},
		{name: "own other opts", file: `{"log-opts":{"labels":"a"}}`, restarts: 1,
			want: map[string]string{"labels": "a", "max-size": "10m", "max-file": "3"}},
		{name: "differing size", file: `{"log-driver":"json-file","log-opts":{"max-size":"50m"}}`, warn: true},
		{name: "other driver", file: `{"log-driver":"journald"}`, warn: true},
		{name: "already set", file: `{"log-driver":"json-file","log-opts":{"max-size":"10m","max-file":"3"}}`},
		{name: "opts set, no driver", file: `{"log-opts":{"max-size":"10m","max-file":"3"}}`},
		{name: "invalid json", file: `{`, warn: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docker", "daemon.json")
			if c.file != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			n := 0
			changed, err := runner{out: &out}.logCap(context.Background(), path, func(context.Context) error { n++; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if n != c.restarts || changed != (c.restarts > 0) {
				t.Fatalf("restarts = %d, changed = %v; want %d", n, changed, c.restarts)
			}
			if got := strings.Contains(out.String(), "warning"); got != c.warn {
				t.Errorf("warning = %v, output %q", got, out.String())
			}
			b, _ := os.ReadFile(path)
			if c.want == nil {
				if string(b) != c.file {
					t.Fatalf("file changed: %s", b)
				}
				return
			}
			var cfg struct {
				Driver  string            `json:"log-driver"`
				Opts    map[string]string `json:"log-opts"`
				Backend string            `json:"firewall-backend"`
			}
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Driver != "json-file" || len(cfg.Opts) != len(c.want) {
				t.Fatalf("got %s", b)
			}
			for k, v := range c.want {
				if cfg.Opts[k] != v {
					t.Errorf("log-opts %s = %q, want %q", k, cfg.Opts[k], v)
				}
			}
			if c.file == pools && (len(dockerPools(path)) != 1 || cfg.Backend != "iptables") {
				t.Errorf("pools lost: %s", b)
			}
			if st, _ := os.Stat(path); c.file != "" && st.Mode().Perm() != 0o600 {
				t.Errorf("mode = %v, want kept 0600", st.Mode().Perm())
			}
		})
	}
}

// A Docker that does not come back with the new file gets the old one again.
func TestLogCapRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	old := `{"debug":true}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	n := 0
	_, err := runner{out: io.Discard}.logCap(context.Background(), path, func(context.Context) error {
		n++
		if n == 1 {
			return errors.New("dockerd did not start")
		}
		return nil
	})
	if err == nil || n != 2 {
		t.Fatalf("err = %v, restarts = %d", err, n)
	}
	if b, _ := os.ReadFile(path); string(b) != old {
		t.Fatalf("not put back: %s", b)
	}
}
