package db_test

import (
	"os"
	"testing"
)

// 023 renames the tile health columns: values survive under the new names,
// exec timings without a command are zeroed, and down restores the old names.
func TestHealthChecksMigration(t *testing.T) {
	db := sliceWorld(t)
	read := func(name string) string {
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	execAll(t, db, []stmt{{read("023_health_checks.down.sql"), nil}})
	tid := "api"
	execAll(t, db, []stmt{
		{`UPDATE tiles SET health_path=' /up ', healthcheck_cmd='', healthcheck_interval_s=7, healthcheck_retries=3 WHERE id=?`, []any{tid}},
		{`UPDATE tiles SET healthcheck_cmd='  ' || char(10) || char(9), healthcheck_interval_s=8, healthcheck_timeout_s=2, healthcheck_retries=3, healthcheck_start_period_s=4 WHERE id='reporter'`, nil},
		{`UPDATE tiles SET healthcheck_cmd='curl -f x' || char(10), healthcheck_interval_s=10, healthcheck_timeout_s=-5, healthcheck_retries=5, healthcheck_start_period_s=6 WHERE id='pg'`, nil},
		{`UPDATE tiles SET healthcheck_cmd='x', healthcheck_interval_s=100000, healthcheck_timeout_s=90000, healthcheck_retries=500, healthcheck_start_period_s=200000 WHERE id='api-db'`, nil},
		{read("023_health_checks.up.sql"), nil},
	})
	var path string
	var iv, retries, status int
	if err := db.QueryRow(`SELECT health_http_path, health_exec_interval_s, health_exec_retries, health_http_status FROM tiles WHERE id=?`, tid).Scan(&path, &iv, &retries, &status); err != nil {
		t.Fatal(err)
	}
	if path != "/up" || iv != 0 || retries != 0 || status != 0 {
		t.Errorf("after up = %q %d %d %d", path, iv, retries, status)
	}
	var cmd string
	var to, sp int
	row := func(id string) {
		t.Helper()
		if err := db.QueryRow(`SELECT health_exec_cmd, health_exec_interval_s, health_exec_timeout_s, health_exec_retries, health_exec_start_period_s FROM tiles WHERE id=?`, id).Scan(&cmd, &iv, &to, &retries, &sp); err != nil {
			t.Fatal(err)
		}
	}
	row("pg")
	if cmd != "curl -f x" || iv != 10 || to != 0 || retries != 5 || sp != 6 {
		t.Errorf("kept exec (negative timeout clamped to 0) = %q %d %d %d %d", cmd, iv, to, retries, sp)
	}
	row("api-db")
	if iv != 86400 || to != 86400 || retries != 100 || sp != 86400 {
		t.Errorf("capped exec = %d %d %d %d, want 86400 86400 100 86400", iv, to, retries, sp)
	}
	row("reporter")
	if iv != 0 || to != 0 || retries != 0 || sp != 0 {
		t.Errorf("blank cmd timings = %d %d %d %d, want zeroed", iv, to, retries, sp)
	}
	execAll(t, db, []stmt{{read("023_health_checks.down.sql"), nil}})
	if err := db.Get(&path, `SELECT health_path FROM tiles WHERE id=?`, tid); err != nil || path != "/up" {
		t.Errorf("after down = %q, %v", path, err)
	}
}
