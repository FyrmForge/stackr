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
