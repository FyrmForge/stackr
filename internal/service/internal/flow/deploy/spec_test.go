package deploy

import (
	"slices"
	"testing"
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
	p, warn := publishedPorts("8080:80\n5353:53/udp\nbad")
	if p["8080"] != "80" || p["5353"] != "53/udp" || len(warn) != 1 {
		t.Errorf("ports = %v, warn %v", p, warn)
	}
	if repoOf("localhost:5000/a/b:1") != "localhost:5000/a/b" || repoOf("nginx@sha256:x") != "nginx" {
		t.Error("repoOf")
	}
}
