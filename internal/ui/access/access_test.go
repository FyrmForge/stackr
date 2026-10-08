package access

import (
	"net/url"
	"testing"
)

func TestStrongest(t *testing.T) {
	lines := []string{
		"a host:/srv:/s", "a port:80", "a lan:all", "a device:/dev/x",
		"b host:/srv:/s", "b host:/var/run/docker.sock:/var/run/docker.sock", "b privileged",
		"c network:host", "c host:/var/run/docker.sock", "c privileged",
		"d host:/a:/b",
	}
	for tile, want := range map[string]string{"a": "device", "b": "docker", "c": "host net", "d": "folder"} {
		if p, ok := Strongest(lines, tile); !ok || p.Chip != want {
			t.Errorf("%s: %q %v, want %q", tile, p.Chip, ok, want)
		}
	}
	if _, ok := Strongest(lines, "zz"); ok {
		t.Error("a tile with no line has a strongest")
	}
	if p, _ := Strongest([]string{"x lan:all", "x port:1"}, "x"); p.Chip != "lan" || p.Detail != "all of the LAN" {
		t.Errorf("lan = %+v", p)
	}
}

func TestRead(t *testing.T) {
	for _, c := range []struct {
		name string
		f    url.Values
		msg  string
		n    int
	}{
		{"none ticked", url.Values{"pending": {"a port:1"}}, NoneTicked, 0},
		{"subset", url.Values{"pending": {"a port:1", "a lan:all"}, "grant": {"a port:1"}}, "", 1},
		{"net needs the name", url.Values{"pending": {"a network:host"}, "grant": {"a network:host"}}, NameWrong, 1},
		{"wrong name", url.Values{"grant": {"a network:host"}, "confirm": {"other"}}, NameWrong, 1},
		{"name typed", url.Values{"grant": {"a host:/var/run/docker.sock"}, "confirm": {"shop"}}, "", 1},
		{"net unticked", url.Values{"pending": {"a network:host", "a port:1"}, "grant": {"a port:1"}}, "", 1},
	} {
		_, g, msg := Read(c.f, "shop")
		if msg != c.msg || len(g) != c.n {
			t.Errorf("%s: %q %v", c.name, msg, g)
		}
	}
}

func TestDockerSocketDetection(t *testing.T) {
	for perm, want := range map[string]bool{
		"host:/var/run/docker.sock:/s": true, "host:/run/docker.sock:/s": true,
		"host:/:/h": true, "host:/var:/h": true, "host:/var/run:/h": true, "host:/run/:/h": true,
		"host:/var/run/docker.sock.bak:/h": true,
		"host:/srv:/s": false, "host:/var/lib:/h": false, "host:/runner:/h": false, "host:/var/runx:/h": false,
	} {
		if got := Parse("a " + perm).Kind == KindDocker; got != want {
			t.Errorf("%s docker = %v, want %v", perm, got, want)
		}
	}
}
