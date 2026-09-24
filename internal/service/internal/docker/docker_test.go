package docker

import (
	"errors"
	"strings"
	"testing"

	"github.com/docker/go-connections/nat"
)

func TestHealthWord(t *testing.T) {
	for status, want := range map[string]string{
		"Up 2 minutes (healthy)":          "healthy",
		"Up 2 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Up 5 seconds":                    "",
		"Exited (1) 3 seconds ago":        "",
	} {
		if got := healthWord(status); got != want {
			t.Errorf("healthWord(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestPortBindings(t *testing.T) {
	exposed, bind := portBindings(map[string]string{"8080": "80", "5353": "53/udp"})
	for _, p := range []nat.Port{"80/tcp", "53/udp"} {
		if _, ok := exposed[p]; !ok {
			t.Errorf("port %s not exposed", p)
		}
	}
	if b := bind["80/tcp"]; len(b) != 1 || b[0].HostPort != "8080" {
		t.Errorf("80/tcp bound to %v", b)
	}
	if e, b := portBindings(nil); e != nil || b != nil {
		t.Error("no ports should give nil sets")
	}
}

func TestIsTarChanged(t *testing.T) {
	for msg, want := range map[string]bool{
		"exit status 1: tar: ./db: file changed as we read it": true,
		"exit status 1: tar: short read":                       true,
		"exit status 2: tar: permission denied":                false,
	} {
		if got := isTarChanged(errors.New(msg)); got != want {
			t.Errorf("isTarChanged(%q) = %v", msg, got)
		}
	}
}

func TestDrainProgress(t *testing.T) {
	stream := `{"status":"Pulling from library/alpine","id":"3"}
{"status":"Downloading","progress":"[==>   ]","id":"abc"}
{"status":"Digest: sha256:00"}
{"errorDetail":{"message":"denied"},"error":"denied"}
{"status":"after the error"}
`
	var log strings.Builder
	err := drainProgress(strings.NewReader(stream), &log)
	if err == nil || err.Error() != "denied" {
		t.Fatalf("stream error lost: %v", err)
	}
	if want := "3: Pulling from library/alpine\nDigest: sha256:00\nafter the error\n"; log.String() != want {
		t.Fatalf("log = %q", log.String())
	}
}

func TestPickDigest(t *testing.T) {
	rds := []string{"mirror/alpine@sha256:bbb", "alpine@sha256:aaa"}
	for ref, want := range map[string]string{
		"alpine:3":                   "sha256:aaa",
		"alpine":                     "sha256:aaa",
		"localhost:5000/other:1":     "sha256:bbb", // fallback: any digest
		"docker.io/library/alpine:3": "sha256:bbb", // no exact repo, first digest
	} {
		if got := pickDigest(ref, rds); got != want {
			t.Errorf("pickDigest(%q) = %q, want %q", ref, got, want)
		}
	}
	if pickDigest("x:1", nil) != "" {
		t.Error("built locally should be empty")
	}
	// A registry port is not a tag.
	if got := pickDigest("reg:5000/app", []string{"x@sha256:1", "reg:5000/app@sha256:2"}); got != "sha256:2" {
		t.Errorf("port read as tag: %q", got)
	}
}
