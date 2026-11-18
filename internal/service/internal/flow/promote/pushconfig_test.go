package promote

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Two stacks read one config repo, each from its own file. A config push
// makes a release only in the stack whose own file, include or mounted file
// changed; a commit the clone lacks makes one (the check cannot tell).
func TestPushConfigOnlyWhereItChanged(t *testing.T) {
	w := setup(t)
	a := w.st
	a.ConfigPath = "a.yml"
	must(t, w.s.Stacks.Update(ctx, a))
	b := a
	b.ID, b.Slug, b.Name, b.ConfigPath = uuid.NewString(), "other", "other", "b.yml"
	must(t, w.s.Stacks.Create(ctx, b))
	envB := w.dev
	envB.ID, envB.StackID = uuid.NewString(), b.ID
	envB.CreatedAt = time.Now()
	must(t, w.s.Environments.Create(ctx, envB))

	fileA := "version: 1\nstack: shop\nladder: [dev]\nhead: main\nbase:\n  tiles:\n    api: {image: nginx:1, port: 80}\n"
	fileB := "version: 1\nstack: other\nladder: [dev]\nhead: main\ninclude: [inc.yml]\nbase:\n  tiles:\n    web: {image: nginx:1, port: 80, files: [\"mount/x:/etc/x\"]}\n"
	w.f.Config = func(_ context.Context, st store.Stack, _ string, _ io.Writer) ([]byte, Fetcher, error) {
		if st.ConfigPath == "b.yml" {
			return []byte(fileB), treeFetch(map[string]string{"inc.yml": "base: {tiles: {}}\n"}), nil
		}
		return []byte(fileA), nil, nil
	}
	ids := map[string]string{}
	w.f.FileID = func(_ context.Context, _ store.Stack, commit, path string) (string, error) {
		if commit == "gone" || commit == "old-gone" {
			return "", errors.New("not in the clone")
		}
		return ids[commit+"|"+path], nil
	}
	// c1 is the base of both; every later commit starts equal to it.
	same := func(commit string, changed ...string) {
		for _, p := range []string{"a.yml", "b.yml", "inc.yml", "mount/x"} {
			ids[commit+"|"+p] = "id-" + p
		}
		for _, p := range changed {
			ids[commit+"|"+p] = "new-" + p
		}
	}
	same("c1")
	for _, st := range []store.Stack{a, b} {
		r, err := w.f.D.Releases.Create(ctx, st.ID, "test", []release.Pin{{Slug: release.ConfigSlug, Repo: st.ConfigRepo, CommitSHA: "c1"}})
		must(t, err)
		e, err := w.f.D.Envs.List(ctx, st.ID)
		must(t, err)
		_, err = w.f.D.Envs.SetRelease(ctx, e[0], r.ID)
		must(t, err)
	}
	push := func(st store.Stack, commit string) bool {
		r, _, err := w.f.Push(ctx, st.ID, Event{Repo: "git@github.com:acme/shop.git", Branch: "main", Commit: commit, Changed: []string{"x"}}, io.Discard)
		must(t, err)
		return r.ID != ""
	}
	for _, c := range []struct {
		name    string
		commit  string
		changed []string
		wantA   bool
		wantB   bool
	}{
		{"only A's file", "c2", []string{"a.yml"}, true, false},
		{"a file B mounts", "c3", []string{"mount/x"}, false, true},
		{"a file B includes", "c4", []string{"inc.yml"}, false, true},
		{"B's own file", "c5", []string{"b.yml"}, false, true},
		{"nothing either reads", "c6", nil, false, false},
	} {
		same(c.commit, c.changed...)
		if got := push(a, c.commit); got != c.wantA {
			t.Errorf("%s: stack A release = %v", c.name, got)
		}
		if got := push(b, c.commit); got != c.wantB {
			t.Errorf("%s: stack B release = %v", c.name, got)
		}
	}
	var log strings.Builder
	if _, _, err := w.f.Push(ctx, a.ID, Event{Repo: "git@github.com:acme/shop.git", Branch: "main", Commit: "c6"}, &log); err != nil || !strings.Contains(log.String(), "no config pin") {
		t.Errorf("log = %q err %v", log.String(), err)
	}
	// The clone lacks the new commit: nothing can be compared, so release.
	if !push(a, "gone") || !push(b, "gone") {
		t.Error("a commit the clone lacks must make a release")
	}
}
