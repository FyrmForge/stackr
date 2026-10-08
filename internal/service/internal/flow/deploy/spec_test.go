package deploy

import (
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestSplitCommandAndPorts(t *testing.T) {
	for in, want := range map[string][]string{
		`--config.file=/etc/p.yml --web.listen-address=":9090"`: {"--config.file=/etc/p.yml", "--web.listen-address=:9090"},
		`sh -c 'echo "hi there"'`:                               {"sh", "-c", `echo "hi there"`},
		`a\ b c`:                                                {"a b", "c"},
	} {
		if got, err := splitCommand(in); err != nil || !slices.Equal(got, want) {
			t.Errorf("splitCommand(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := splitCommand(`echo "open`); err == nil {
		t.Error("unterminated quote accepted")
	}
	p, warn := publishedPorts("8080:80\n5353:53/udp\n53:53\n53:53/udp\nbad")
	if p["8080"] != "80" || p["5353/udp"] != "53/udp" || p["53"] != "53" || p["53/udp"] != "53/udp" || len(warn) != 1 {
		t.Errorf("ports = %v, warn %v", p, warn)
	}
	if RepoOf("localhost:5000/a/b:1") != "localhost:5000/a/b" || RepoOf("nginx@sha256:x") != "nginx" {
		t.Error("repoOf")
	}
}

// A replica names the ref it was started for: the pinned one when the deploy
// knows it, else the image it runs.
func TestSpecRefLabel(t *testing.T) {
	got := spec(store.Tile{}, resolved{image: "nginx:1", pinRef: "nginx@sha256:one"}).Labels[LabelRef]
	if got != "nginx@sha256:one" {
		t.Errorf("label = %q; want the pinned ref", got)
	}
	got = spec(store.Tile{}, resolved{image: "nginx:1"}).Labels[LabelRef]
	if got != "nginx:1" {
		t.Errorf("label = %q; want the image", got)
	}
}
