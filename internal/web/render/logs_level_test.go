package render

import "testing"

func TestLogLevel(t *testing.T) {
	for _, c := range []struct{ text, stream, want string }{
		{"panic: runtime error", "stdout", "error"},
		{"fatal error: all goroutines are asleep", "stdout", "error"},
		{"Traceback (most recent call last):", "stdout", "error"},
		{"Exception in thread main", "stdout", "error"},
		{`{"level":50,"msg":"x"}`, "stdout", "error"},
		{`{"level":"warn","msg":"x"}`, "stdout", "warn"},
		{`{"severity":"CRITICAL"}`, "stdout", "error"},
		{`{"@l":"Information"}`, "stderr", ""},
		{`{"level":40}`, "stdout", "warn"},
		{`{"level":30}`, "stderr", ""},
		{`{"msg":"no level"}`, "stderr", "error"},
		{`time=1 level=error msg=x`, "stdout", "error"},
		{`level="warning" msg=x`, "stdout", "warn"},
		{`level=info msg=x`, "stderr", ""},
		{"[Error] DownloadedEpisodesImportService: Import failed", "stdout", "error"},
		{"2026-11-24 09:00:00 [Warn] slow", "stdout", "warn"},
		{"[v4.0.20.3014] System.ArgumentNullException: x", "stdout", ""},
		{"[v4.0.20.3014] System.ArgumentNullException: x", "stderr", "error"},
		{"[Info] started", "stderr", ""},
		{"2026/11/24 [core:error] [pid 1] AH001", "stdout", "error"},
		{"[emerg] 1#1: bind failed", "stdout", "error"},
		{"ERROR: db down", "stdout", "error"},
		{"2026-11-24 12:00:00 WARNING disk low", "stdout", "warn"},
		{"INFO started", "stderr", ""},
		{"E1124 09:12:07.123 foo.go:1] bad", "stdout", "error"},
		{"W1124 09:12:07.123 foo.go:1] hmm", "stdout", "warn"},
		{"I1124 09:12:07.123 foo.go:1] ok", "stderr", ""},
		{"no error found", "stdout", ""},
		{"plain line", "stdout", ""},
		{"\x1b[34mINFO\x1b[0m server started", "stderr", ""},
		{"\x1b[31mERROR\x1b[0m db down", "stdout", "error"},
		{"2026-11-24 10:00:00.123 UTC [1] LOG:  database system is ready", "stderr", ""},
		{"NOTICE:  table x does not exist", "stderr", ""},
		{"DETAIL:  something", "stderr", ""},
		{"HINT:  try", "stderr", ""},
		{"STATEMENT:  select 1", "stderr", ""},
		{"CONTEXT:  here", "stderr", ""},
		{`{"severity":200}`, "stdout", ""},
		{`{"level":3}`, "stderr", "error"},
		{`{"level":3}`, "stdout", ""},
		{`{"level":70}`, "stdout", ""},
		{"Error: connect ECONNREFUSED 127.0.0.1:5432", "stdout", "error"},
		{"2026-11-24T10:00:00Z error: boom", "stdout", "error"},
		{"Unhandled exception. System.Exception: x", "stdout", "error"},
		{"no error: found", "stdout", ""},
		{"plain line", "stderr", "error"},
	} {
		if got := logLevel(c.text, c.stream); got != c.want {
			t.Errorf("logLevel(%q, %s) = %q, want %q", c.text, c.stream, got, c.want)
		}
	}
}
