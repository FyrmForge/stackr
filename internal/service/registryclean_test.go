package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// fakeRegistry answers the slice of the registry API the sweep uses: catalog,
// tags, manifest HEAD by tag and manifest DELETE by digest. onDelete sees
// every delete.
type fakeRegistry struct {
	mu       sync.Mutex
	tags     map[string]map[string]string // repo -> tag -> digest
	onDelete func()
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case p == "_catalog":
		var repos []string
		for repo := range f.tags {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
	case strings.HasSuffix(p, "/tags/list"):
		repo := strings.TrimSuffix(p, "/tags/list")
		var tags []string
		for tag := range f.tags[repo] {
			tags = append(tags, tag)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
	case strings.Contains(p, "/manifests/"):
		repo, ref, _ := strings.Cut(p, "/manifests/")
		switch r.Method {
		case http.MethodDelete:
			if f.onDelete != nil {
				f.onDelete()
			}
			for tag, d := range f.tags[repo] {
				if d == ref {
					delete(f.tags[repo], tag)
				}
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			d, ok := f.tags[repo][ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", d)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistry) has(repo, tag string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tags[repo][tag]
	return ok
}

// The sweep keeps the newest registry_keep per repo and what an env uses (a
// built image its release pins, an imported one a tile names), deletes the
// rest with its rows while the gate is shut, runs the garbage collect and
// opens the gate; the result is kept and in the security log.
func TestRegistrySweep(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	const web, pg = "acme/shop/web", "acme/docker.io/library/postgres"
	fr := &fakeRegistry{tags: map[string]map[string]string{
		web:                    {"b1": "sha256:b1", "b2": "sha256:b2", "b3": "sha256:b3", "b4": "sha256:b4"},
		"stackr.system/alpine": {"3": "sha256:s3", "2": "sha256:s2"},
		pg:                     {"14": "sha256:p14", "15": "sha256:p15", "16": "sha256:p16", "latest": "sha256:p16"},
	}}
	srv := httptest.NewServer(fr)
	defer srv.Close()
	w.orch.regs.WithBase(srv.URL)
	fr.onDelete = func() {
		if !w.orch.regs.Gate().Closed() {
			t.Error("a version was deleted with the gate open")
		}
	}

	// Rows, oldest first; each version 100 bytes.
	t0 := time.Now().Add(-time.Hour)
	org := w.org
	add := func(repo, tag, digest string, i int) {
		must(t, w.orch.regs.Record(ctx, store.RegistryImage{
			OrgID: &org, Repo: repo, Tag: tag, Digest: digest, Source: registry.SourceBuilt,
			Size: 100, CreatedAt: t0.Add(time.Duration(i) * time.Minute),
		}))
	}
	for i, tag := range []string{"b1", "b2", "b3", "b4"} {
		add(web, tag, "sha256:"+tag, i)
	}
	for i, tag := range []string{"14", "15", "16"} {
		add(pg, tag, "sha256:p"+tag, i)
	}
	// b1 is what the env's release pins (its ref in the registry), 15 what a tile names.
	im, err := w.orch.images.Built(ctx, image.ClusterRegistry+web+":b1", "sha256:unrelated")
	must(t, err)
	rel := w.release(t, im.ID)
	e, err := w.st.Environments.Get(ctx, w.env)
	must(t, err)
	e.ReleaseID = &rel
	must(t, w.st.Environments.Update(ctx, e))
	_, err = w.orch.CreateTile(ctx, Tile{
		StackID: w.stack, EnvironmentID: w.env, Name: "db", Kind: "image", ImageRef: "postgres:15", ContainerPort: 5432,
	})
	must(t, err)
	must(t, w.orch.SetSetting(ctx, "registry_keep", "1"))
	w.fake.ExecOut = "512\t/var/lib/registry\n"

	j, err := w.orch.CleanRegistryNow(ctx, "u1")
	must(t, err)
	if done := w.wait(t, j.ID); done.State != job.Done {
		t.Fatalf("job = %s: %s", done.State, done.Error)
	}

	for _, c := range []struct {
		repo, tag string
		want      bool
	}{
		{web, "b1", true}, {web, "b2", false}, {web, "b3", false}, {web, "b4", true},
		{"stackr.system/alpine", "2", true}, {pg, "14", false}, {pg, "15", true}, {pg, "16", true}, {pg, "latest", true},
	} {
		if got := fr.has(c.repo, c.tag); got != c.want {
			t.Errorf("%s:%s present = %v, want %v", c.repo, c.tag, got, c.want)
		}
	}
	rows, err := w.st.RegistryImages.ListAll(ctx)
	must(t, err)
	if len(rows) != 4 { // b1, b4, 15, 16
		t.Errorf("rows left = %d: %+v", len(rows), rows)
	}
	if w.orch.regs.Gate().Closed() {
		t.Error("gate still shut after the sweep")
	}
	var gc bool
	for _, c := range w.fake.Calls() {
		gc = gc || (c.Method == "Exec" && strings.Contains(c.String(), "garbage-collect, --delete-untagged"))
	}
	if !gc {
		t.Errorf("no garbage collect run: %v", w.fake.Calls())
	}
	res, ok := w.orch.LastRegistryClean()
	if !ok || res.Deleted != 3 || !res.Manual || len(res.Errors) != 0 || !res.Measured {
		t.Errorf("result = %+v, %v", res, ok)
	}
	evs, err := w.orch.SecurityLog(ctx, SecurityFilter{Action: securitylog.RegistryClean})
	if err != nil || len(evs) != 1 || evs[0].ActorID != "u1" || !strings.Contains(evs[0].Detail, "3 versions deleted") {
		t.Errorf("security log = %+v, %v", evs, err)
	}
}

// A garbage collect that fails still opens the gate and is reported.
func TestRegistrySweepFailureOpensGate(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	srv := httptest.NewServer(&fakeRegistry{tags: map[string]map[string]string{}})
	defer srv.Close()
	w.orch.regs.WithBase(srv.URL)
	w.fake.Err = map[string]error{"Exec": context.DeadlineExceeded}

	j, err := w.orch.CleanRegistryNow(ctx, "")
	must(t, err)
	if done := w.wait(t, j.ID); done.State != job.Failed {
		t.Fatalf("job = %s", done.State)
	}
	if w.orch.regs.Gate().Closed() {
		t.Error("gate shut after a failed sweep")
	}
	if res, ok := w.orch.LastRegistryClean(); !ok || len(res.Errors) == 0 {
		t.Errorf("result = %+v, %v", res, ok)
	}
}
