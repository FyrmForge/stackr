package promote

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"go.yaml.in/yaml/v3"
)

const healthBlock = `    api:
      image: nginx:1
      port: 80
      health:
        http:
          path: /api/health
          status: 2xx
          interval: 30
          timeout: 3
          retries: 3
        exec:
          cmd: curl -f localhost
          interval: 10
          timeout: 2
          retries: 4
          start_period: 10
`

// A full health block of both kinds parses, reaches the row, and exports back
// to a block that parses to the same row (2xx stays 2xx, 200 stays 200).
func TestHealthBlockRoundTrip(t *testing.T) {
	r, err := Load([]byte(tiles(healthBlock)), nil, "acme")
	must(t, err)
	row := toRow("api", r.Envs["production"].Tiles["api"], store.Stack{}, store.Environment{})
	if row.HealthHTTPPath != "/api/health" || row.HealthHTTPStatus != 2 || row.HealthHTTPIntervalS != 30 ||
		row.HealthHTTPTimeoutS != 3 || row.HealthHTTPRetries != 3 || row.HealthExecCmd != "curl -f localhost" ||
		row.HealthExecIntervalS != 10 || row.HealthExecTimeoutS != 2 || row.HealthExecRetries != 4 || row.HealthExecStartPeriodS != 10 {
		t.Fatalf("row = %+v", row)
	}
	for _, status := range []int{2, 200} {
		row.HealthHTTPStatus = status
		out, err := yaml.Marshal(omap{{"health", exportHealth(row)}})
		must(t, err)
		want := "status: 200"
		if status == 2 {
			want = "status: 2xx"
		}
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
		r2, err := Load([]byte(tiles("    api:\n      image: nginx:1\n"+indent(string(out), "      "))), nil, "acme")
		must(t, err)
		if got := toRow("api", r2.Envs["production"].Tiles["api"], store.Stack{}, store.Environment{}); got.HealthHTTPStatus != status ||
			got.HealthExecStartPeriodS != 10 || got.HealthHTTPPath != row.HealthHTTPPath {
			t.Errorf("round trip status %d: %+v", status, got)
		}
	}
	if len(exportHealth(store.Tile{})) != 0 {
		t.Error("an unset health exports a block")
	}
}

func indent(s, p string) string {
	return p + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+p) + "\n"
}

func TestHealthRefusals(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"old health_path":   {"health_path: /x", "use health:"},
		"old healthcheck":   {"healthcheck: ls", "use health:"},
		"old interval":      {"healthcheck_interval: 5", "use health:"},
		"old timeout":       {"healthcheck_timeout: 5", "use health:"},
		"old retries":       {"healthcheck_retries: 5", "use health:"},
		"old start period":  {"healthcheck_start_period: 5", "use health:"},
		"empty http":        {"health: {http: {}}", "health.http needs a path"},
		"empty exec":        {"health: {exec: {}}", "health.exec needs a cmd"},
		"blank path":        {"health: {http: {path: \"\"}}", "health.http needs a path"},
		"space path":        {"health: {http: {path: \" \"}}", "health.http needs a path"},
		"space cmd":         {"health: {exec: {cmd: \" \"}}", "health.exec needs a cmd"},
		"http scalar":       {"health: {http: /x}", "health.http is a block with path:"},
		"exec scalar":       {"health: {exec: ls}", "health.exec is a block with cmd:"},
		"http list":         {"health: {http: [/x]}", "health.http is a block with path:"},
		"health scalar":     {"health: ls", "health is a block with http: or exec:"},
		"blank cmd":         {"health: {exec: {cmd: \"\"}}", "health.exec needs a cmd"},
		"http start period": {"health: {http: {path: /x, start_period: 5}}", "health.http.start_period is not supported yet"},
		"status 2":          {"health: {http: {path: /x, status: 2}}", "use a status code like 200 or a class like 2xx"},
		"status 6xx":        {"health: {http: {path: /x, status: 6xx}}", "use a status code like 200 or a class like 2xx"},
		"status abc":        {"health: {http: {path: /x, status: abc}}", "use a status code like 200 or a class like 2xx"},
		"status 99":         {"health: {http: {path: /x, status: 99}}", "use a status code like 200 or a class like 2xx"},
		"status 600":        {"health: {http: {path: /x, status: 600}}", "use a status code like 200 or a class like 2xx"},
	} {
		_, err := Load([]byte(tiles("    api:\n      image: nginx:1\n      "+c.body+"\n")), nil, "acme")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	// a null half still means no check
	_, err := Load([]byte(tiles("    api:\n      image: nginx:1\n      health: {http: null, exec: ~}\n")), nil, "acme")
	must(t, err)
	// exec.start_period stays legal
	_, err = Load([]byte(tiles("    api:\n      image: nginx:1\n      health: {exec: {cmd: ls, start_period: 5}}\n")), nil, "acme")
	must(t, err)
}

func TestHealthStatusMapping(t *testing.T) {
	for in, want := range map[string]HealthStatus{"2xx": 2, "5xx": 5, "200": 200, "599": 599, `"200"`: 200, "2XX": 2, `"5XX"`: 5} {
		var s HealthStatus
		must(t, yaml.Unmarshal([]byte(in), &s))
		if s != want {
			t.Errorf("%s = %d, want %d", in, s, want)
		}
	}
	if HealthStatus(2).String() != "2xx" || HealthStatus(200).String() != "200" {
		t.Error("status strings")
	}
}

// A changed health.http field plans one health.http line with the block.
func TestPlanShowsHealthBlock(t *testing.T) {
	w := syncWorld(t)
	d := imgTile("nginx:1", 80)
	d.HealthHTTPPath, d.HealthHTTPStatus, d.HealthHTTPIntervalS, d.HealthHTTPTimeoutS, d.HealthHTTPRetries = "/api/health", 2, 30, 3, 3
	w.mk(t, w.dev, "api", d)
	p := imgTile("nginx:1", 80)
	p.HealthHTTPPath, p.HealthHTTPStatus, p.HealthHTTPIntervalS, p.HealthHTTPTimeoutS, p.HealthHTTPRetries = "/api/health", 2, 10, 3, 3
	w.mk(t, w.prd, "api", p)
	for _, c := range w.syncPlan(t).Changes {
		if c.Field == "health.http" {
			if c.Old != "/api/health 2xx every 10s, 3s, 3 tries" || c.New != "/api/health 2xx every 30s, 3s, 3 tries" {
				t.Errorf("health.http = %+v", c)
			}
			return
		}
	}
	t.Error("no health.http change")
}

// A YAML `cmd: |` block ends in a newline; the row stores it trimmed, like a
// panel Save, so the next apply sees no health.exec change.
func TestHealthCmdBlockTrimmed(t *testing.T) {
	src := "    api:\n      image: nginx:1\n      port: 80\n      health:\n        exec:\n          cmd: |\n            curl -f localhost\n"
	r, err := Load([]byte(tiles(src)), nil, "acme")
	must(t, err)
	row := toRow("api", r.Envs["production"].Tiles["api"], store.Stack{}, store.Environment{})
	if row.HealthExecCmd != "curl -f localhost" {
		t.Errorf("cmd = %q", row.HealthExecCmd)
	}
}
