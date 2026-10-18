package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// alerts set reads the secret from stdin, never a flag.
func TestAlertsSetSecretStdin(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	var out, errw bytes.Buffer
	args := []string{"admin", "alerts", "set", "--url", "https://hook.example/x", "--secret-stdin"}
	if code := run(context.Background(), args, strings.NewReader("s3cret\n"), &out, &errw, false); code != 0 {
		t.Fatalf("code %d: %s", code, errw.String())
	}
	want := `PUT /api/v1/admin/alerts {"secret":"s3cret","url":"https://hook.example/x"}`
	if len(r.reqs) != 1 || strings.TrimSpace(r.reqs[0]) != want {
		t.Errorf("reqs = %q", r.reqs)
	}
}
