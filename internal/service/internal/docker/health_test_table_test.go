package docker

import (
	"slices"
	"testing"
)

func TestHealthTest(t *testing.T) {
	shell := func(c string) []string { return []string{"CMD-SHELL", c} }
	for _, tc := range []struct {
		cmd  string
		want []string
	}{
		{"/app/blocky healthcheck", []string{"CMD", "/app/blocky", "healthcheck"}},
		{"pg_isready -q -h /x -p 6432", []string{"CMD", "pg_isready", "-q", "-h", "/x", "-p", "6432"}},
		{"curl -f http://x:80/h", []string{"CMD", "curl", "-f", "http://x:80/h"}},
		{"wget --spider a=b", []string{"CMD", "wget", "--spider", "a=b"}},
		{"redis-cli", []string{"CMD", "redis-cli"}},
		{"curl -f http://x || exit 1", shell("curl -f http://x || exit 1")},
		{"sh -c 'x'", shell("sh -c 'x'")},
		{`echo "a"`, shell(`echo "a"`)},
		{"cat $HOME/x", shell("cat $HOME/x")},
		{"ls ~", shell("ls ~")},
		{"ls /x/*", shell("ls /x/*")},
		{"ls /x/?", shell("ls /x/?")},
		{"ls !x", shell("ls !x")},
		{"a;b", shell("a;b")},
		{"a|b", shell("a|b")},
		{"a>b", shell("a>b")},
		{"a&b", shell("a&b")},
		{"a `b`", shell("a `b`")},
		{"a (b)", shell("a (b)")},
		{"a\nb", shell("a\nb")},
		{"a\tb", shell("a\tb")},
		{"a  b", shell("a  b")},
		{" a", shell(" a")},
		{"a ", shell("a ")},
		{"a\\ b", shell("a\\ b")},
		{"é", shell("é")},
		{"FOO=1 cmd", shell("FOO=1 cmd")},
		{"FOO=1", shell("FOO=1")},
		{"test -f /x", shell("test -f /x")},
		{"[ -f /x ]", shell("[ -f /x ]")},
		{"true", shell("true")},
		{"false", shell("false")},
		{": x", shell(": x")},
		{". /x", shell(". /x")},
		{"exit 1", shell("exit 1")},
		{"if x", shell("if x")},
		{"do x", shell("do x")},
		{"command -v x", shell("command -v x")},
		{"printf x", shell("printf x")},
		{"/usr/bin/test -f /x", []string{"CMD", "/usr/bin/test", "-f", "/x"}},
	} {
		if got := healthTest(tc.cmd); !slices.Equal(got, tc.want) {
			t.Errorf("%q = %q, want %q", tc.cmd, got, tc.want)
		}
	}
	for _, w := range shellFirst {
		if got := healthTest(w + " x"); got[0] != "CMD-SHELL" {
			t.Errorf("builtin %q went exec", w)
		}
	}
}
