package main

import (
	"strings"
	"testing"
)

// An empty value for a tile list flag clears the list, spaced or with "=".
func TestTileSetEmptyListFlagClears(t *testing.T) {
	for _, args := range [][]string{{"--published-ports", ""}, {"--published-ports="}} {
		r := fake(t, map[string]string{"/tiles/api": `{"tile":{"slug":"api"}}`})
		a := append([]string{"tile", "set", "api", "--stack", "shop", "--env", "dev"}, args...)
		code, _, errw := cli(t, a...)
		w := writes(r)
		if code != 0 || len(w) != 1 || !strings.Contains(w[0], `"published_ports":""`) {
			t.Errorf("%q = %d %s %v, want a clear", args, code, errw, w)
		}
	}
}

func TestHealthStatusFlag(t *testing.T) {
	for in, want := range map[string]int{"200": 200, "599": 599, "2xx": 2, "1xx": 1, "5xx": 5, "": 0, "2XX": 2} {
		if got, err := healthStatus(in); err != nil || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"2", "6xx", "0xx", "199", "600", "abc"} {
		if _, err := healthStatus(in); err == nil || !strings.Contains(err.Error(), "like 200 or a class like 2xx") {
			t.Errorf("%q: err = %v, want refused", in, err)
		}
	}
}

func TestTileSetHealthFlags(t *testing.T) {
	r := fake(t, map[string]string{"/tiles/api": `{"tile":{"slug":"api"}}`})
	code, _, errw := cli(t, "tile", "set", "api", "--stack", "shop", "--env", "dev",
		"--health-http-path", "/up", "--health-http-status", "2xx", "--health-http-interval", "30",
		"--health-http-timeout", "3", "--health-http-retries", "4", "--health-exec-cmd", "true",
		"--health-exec-interval", "10", "--health-exec-timeout", "2", "--health-exec-retries", "5",
		"--health-exec-start-period", "7")
	w := writes(r)
	if code != 0 || len(w) != 1 {
		t.Fatalf("code %d %s %v", code, errw, w)
	}
	for _, s := range []string{`"health_http_path":"/up"`, `"health_http_status":2`, `"health_http_interval_s":30`,
		`"health_http_timeout_s":3`, `"health_http_retries":4`, `"health_exec_cmd":"true"`,
		`"health_exec_interval_s":10`, `"health_exec_timeout_s":2`, `"health_exec_retries":5`, `"health_exec_start_period_s":7`} {
		if !strings.Contains(w[0], s) {
			t.Errorf("body %s lacks %s", w[0], s)
		}
	}
	r = fake(t, map[string]string{"/tiles/api": `{}`})
	code, _, errw = cli(t, "tile", "set", "api", "--stack", "shop", "--env", "dev", "--health-http-status", "6xx")
	if code == 0 || len(writes(r)) != 0 || !strings.Contains(errw, "like 200 or a class like 2xx") {
		t.Errorf("bad status = %d %s, want refused", code, errw)
	}
}
