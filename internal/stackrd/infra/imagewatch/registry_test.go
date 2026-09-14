package imagewatch

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		ref                 string
		host, repoPath, tag string
		ok                  bool
	}{
		{"nginx", "registry-1.docker.io", "library/nginx", "latest", true},
		{"nginx:1.27", "registry-1.docker.io", "library/nginx", "1.27", true},
		{"grafana/grafana:10.4", "registry-1.docker.io", "grafana/grafana", "10.4", true},
		{"docker.io/nginx", "registry-1.docker.io", "library/nginx", "latest", true},
		{"ghcr.io/owner/app:main", "ghcr.io", "owner/app", "main", true},
		{"localhost:5000/app", "localhost:5000", "app", "latest", true},
		{"registry.example.com:8443/team/app:v2", "registry.example.com:8443", "team/app", "v2", true},
		{"nginx@sha256:deadbeef", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, c := range cases {
		host, repoPath, tag, ok := ParseRef(c.ref)
		if host != c.host || repoPath != c.repoPath || tag != c.tag || ok != c.ok {
			t.Errorf("ParseRef(%q) = %q %q %q %v, want %q %q %q %v",
				c.ref, host, repoPath, tag, ok, c.host, c.repoPath, c.tag, c.ok)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`)
	if scheme != "bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" {
		t.Errorf("got %q %v", scheme, p)
	}
	if s, _ := parseChallenge(`Basic realm="registry"`); s != "basic" {
		t.Errorf("basic scheme got %q", s)
	}
}
