package docker

import (
	"errors"
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
