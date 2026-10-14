package main

import "testing"

func TestDevModeOnPublicHost(t *testing.T) {
	for _, c := range []struct {
		dev  bool
		url  string
		want bool
	}{
		{true, "", false},
		{true, "http://localhost:8080", false},
		{true, "http://127.0.0.1:8080", false},
		{true, "http://[::1]:8080", false},
		{true, "http://app.localhost", false},
		{true, "https://stackr.example.com", true},
		{true, "http://192.168.1.5", true},
		{false, "https://stackr.example.com", false},
	} {
		if got := devModeOnPublicHost(c.dev, c.url); got != c.want {
			t.Errorf("%v %q = %v, want %v", c.dev, c.url, got, c.want)
		}
	}
}
