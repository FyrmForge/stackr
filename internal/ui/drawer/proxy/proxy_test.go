package proxy

import (
	"context"
	"strings"
	"testing"
)

func TestRoutesRenders(t *testing.T) {
	var b strings.Builder
	err := Routes(View{
		Rows: []Route{{
			Host: "api.acme.dev",
			Path: "/",
			Tile: "api",
			Port: "80",
			Raw:  true,
		}},
	}).Render(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"api.acme.dev/", "api:80", "http only", "raw Caddy"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q", w)
		}
	}
}

func TestRoutesShowGate(t *testing.T) {
	var b strings.Builder
	err := Routes(View{Rows: []Route{
		{Host: "a.io", Tile: "a", Port: "80", Gate: "alice"},
		{Host: "b.io", Tile: "b", Port: "80", Gate: "alice", Lock: "gate.password is not set"},
	}}).Render(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"gate: alice", "locked", "Locked for everyone: gate.password is not set."} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q", w)
		}
	}
}
