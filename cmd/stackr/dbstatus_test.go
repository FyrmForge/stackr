package main

import (
	"strings"
	"testing"
)

// db status prints the headline, standby lag and pooler clients; --top adds
// statements; an instance with no sample yet says so in one line.
func TestDBStatus(t *testing.T) {
	r := fake(t, map[string]string{"/tiles/pg/health": `{"sampled":true,"at":"2026-11-11T10:00:00Z","conn_used":12,"conn_max":100,` +
		`"oldest_xact_s":90,"archiving":true,"standbys":[{"name":"s1","lag_seconds":2,"lag_bytes":2048}],"slot_wal":[],` +
		`"top":[{"calls":5,"total_ms":40,"mean_us":8000,"rows":5,"text":"select 1"}],"top_error":"",` +
		`"pooler":{"status":"running","host":"h","port":6432,"active":3,"waiting":1,"counted":true}}`})
	code, out, errw := cli(t, "db", "status", "--tile", "pg", "--stack", "shop", "--env", "dev")
	if code != 0 {
		t.Fatalf("db status = %d %q", code, errw)
	}
	for _, w := range []string{"connections    12 of 100", "oldest xact    1m30s", "archiving      on",
		"running, 3 clients active, 1 waiting", "s1\t2 s\t2.0 KB"} {
		if !strings.Contains(out, w) {
			t.Errorf("no %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "select 1") {
		t.Errorf("statements without --top:\n%s", out)
	}
	if !strings.Contains(r.reqs[0], "GET /api/v1/orgs/acme/stacks/shop/envs/dev/tiles/pg/health ") {
		t.Errorf("reqs = %v", r.reqs)
	}
	_, out, _ = cli(t, "db", "status", "--tile", "pg", "--stack", "shop", "--env", "dev", "--top")
	if !strings.Contains(out, "select 1") {
		t.Errorf("--top lacks the statement:\n%s", out)
	}

	fake(t, map[string]string{"/tiles/pg/health": `{"sampled":false}`})
	_, out, _ = cli(t, "db", "status", "--tile", "pg", "--stack", "shop", "--env", "dev")
	if strings.TrimSpace(out) != "not sampled yet" {
		t.Errorf("unsampled = %q", out)
	}
}
