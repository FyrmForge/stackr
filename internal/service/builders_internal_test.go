package service

import (
	"errors"
	"io"
	"slices"
	"testing"
)

// The builder with the most available memory builds; the controller wins
// ties and builds when no node is tagged.
func TestPickBuilder(t *testing.T) {
	for _, c := range []struct {
		name  string
		cands []builderCand
		want  string
	}{
		{name: "none tagged", want: "ctl"},
		{name: "controller only", cands: []builderCand{{"ctl", 4}}, want: "ctl"},
		{name: "roomier worker", cands: []builderCand{{"ctl", 4}, {"w1", 8}, {"w2", 6}}, want: "w1"},
		{name: "tie", cands: []builderCand{{"w1", 4}, {"ctl", 4}}, want: "ctl"},
		{name: "controller untagged", cands: []builderCand{{"w1", 2}}, want: "w1"},
		{name: "facts unknown", cands: []builderCand{{"w1", 0}}, want: "ctl"},
	} {
		if got := pickBuilder("ctl", c.cands); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A worker that drops out mid-build hands the build to the controller; a
// build that fails on a worker still answering keeps its error.
func TestBuildFallback(t *testing.T) {
	fail := errors.New("build failed")
	for _, c := range []struct {
		name    string
		at      string
		failOn  []string
		answers bool
		want    []string
		wantErr bool
	}{
		{name: "worker builds", at: "w1", want: []string{"w1"}},
		{name: "worker dropped", at: "w1", failOn: []string{"w1"}, want: []string{"w1", "ctl"}},
		{name: "build broken", at: "w1", failOn: []string{"w1"}, answers: true, want: []string{"w1"}, wantErr: true},
		{name: "controller fails", at: "ctl", failOn: []string{"ctl"}, want: []string{"ctl"}, wantErr: true},
	} {
		var ran []string
		_, err := buildFallback(c.at, "ctl", func(n string) (string, error) {
			ran = append(ran, n)
			if slices.Contains(c.failOn, n) {
				return "", fail
			}
			return "img", nil
		}, func(string) bool { return c.answers }, io.Discard)
		if (err != nil) != c.wantErr || !slices.Equal(ran, c.want) {
			t.Errorf("%s: ran %v err %v, want %v", c.name, ran, err, c.want)
		}
	}
}
