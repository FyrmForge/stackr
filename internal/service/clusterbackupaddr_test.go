package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEndpointAddr(t *testing.T) {
	old := resolveHost
	defer func() { resolveHost = old }()
	resolveHost = func(_ context.Context, h string) ([]string, error) {
		if h == "nas.lan" {
			return []string{"192.168.1.50"}, nil
		}
		return nil, errors.New("no such host")
	}
	for in, want := range map[string]string{
		"http://192.168.1.50:9000": "192.168.1.50",
		"https://gone.example":     "",
		"":                         "",
	} {
		if got := endpointAddr(context.Background(), in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	// A name is not looked up on the render path: the first call answers "",
	// the background lookup fills the cache.
	if got := endpointAddr(context.Background(), "https://nas.lan"); got != "" {
		t.Errorf("first call %q, want empty", got)
	}
	for i := 0; i < 200 && endpointAddr(context.Background(), "https://nas.lan") == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := endpointAddr(context.Background(), "https://nas.lan"); got != "192.168.1.50" {
		t.Errorf("after lookup %q", got)
	}
}
