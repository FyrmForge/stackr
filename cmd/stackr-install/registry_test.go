package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// No registry: it runs. A stale one (the fake answers "old" for the label) is
// removed first.
func TestEnsureRegistry(t *testing.T) {
	for have, want := range map[string]string{
		"":                "run -d",
		"stackr-registry": "rm -f|run -d",
	} {
		log := fakeDocker(t, have, false)
		if err := (runner{out: &bytes.Buffer{}}).ensureRegistry(context.Background(), t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(proxyCalls(t, log), "|"); got != want {
			t.Errorf("have %q: calls %q, want %q", have, got, want)
		}
	}
}
