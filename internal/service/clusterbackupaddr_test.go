package service

import (
	"context"
	"errors"
	"testing"
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
		"https://nas.lan":          "192.168.1.50",
		"https://gone.example":     "",
		"":                         "",
	} {
		if got := endpointAddr(context.Background(), in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
