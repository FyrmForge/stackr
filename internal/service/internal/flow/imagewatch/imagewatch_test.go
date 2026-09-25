package imagewatch

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

var ctx = context.Background()

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error {
	return nil
}

func (vipStub) Remove(context.Context, string) error {
	return nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fakeRegistry answers digests per ref and tags per repo, and counts calls.
type fakeRegistry struct {
	digests map[string]string
	tags    map[string][]string
	fail    map[string]bool
	calls   []string
}

func (r *fakeRegistry) Digest(_ context.Context, ref, _, _ string) (string, error) {
	r.calls = append(r.calls, "digest "+ref)
	if r.fail[ref] {
		return "", errors.New("503 from registry")
	}
	return r.digests[ref], nil
}

func (r *fakeRegistry) Tags(_ context.Context, ref, _, _ string) ([]string, error) {
	r.calls = append(r.calls, "tags "+ref)
	return r.tags[ref], nil
}

func TestCheck(t *testing.T) {
	s := storetest.Store(t)
	fake := dockerfake.New()
	reg := &fakeRegistry{
		digests: map[string]string{"nginx:1": "sha256:n1", "redis:7.2.0": "sha256:r72"},
		tags:    map[string][]string{"redis": {"7.0.1", "7.2.0", "8.0.0", "7.3.0-rc1", "6.9"}},
	}
	f := &Flow{
		Orgs:     org.New(s.Orgs, s.OrgMembers, s.Invites),
		Stacks:   stack.New(s.Stacks),
		Envs:     environment.New(s.Environments, fake),
		Tiles:    tile.New(s.Tiles, fake, vipStub{}),
		Images:   image.New(s.Images, fake),
		Releases: release.New(s.Releases, s.ReleaseTiles),
		Creds:    credential.New(s.Credentials),
		Settings: settings.New(s.Settings, nil),
		Digest:   reg.Digest,
		Tags:     reg.Tags,
	}

	now := time.Now()
	o, stID := uuid.NewString(), uuid.NewString()
	must(t, s.Orgs.Create(ctx, store.Org{
		ID:        o,
		Name:      "o",
		Slug:      "o",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, s.Stacks.Create(ctx, store.Stack{
		ID:        stID,
		OrgID:     o,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		CreatedAt: now,
	}))
	dev := store.Environment{
		ID:         uuid.NewString(),
		StackID:    stID,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}
	prd := store.Environment{
		ID:        uuid.NewString(),
		StackID:   stID,
		Name:      "prd",
		Slug:      "prd",
		Type:      "static",
		Settings:  "{}",
		Network:   "n",
		Position:  1,
		FromKind:  "promote",
		CreatedAt: now,
	}
	must(t, s.Environments.Create(ctx, dev))
	must(t, s.Environments.Create(ctx, prd))
	mk := func(env, name, ref, update, tag string) {
		_, err := f.Tiles.Create(ctx, store.Tile{
			StackID:       stID,
			EnvironmentID: env,
			Name:          name,
			Kind:          tile.Image,
			ImageRef:      ref,
			UpdatePolicy:  update,
			TagPolicy:     tag,
		})
		must(t, err)
	}
	mk(dev.ID, "api", "nginx:1", "auto", "")
	mk(dev.ID, "web", "nginx:1", "auto", "")               // same image: no second call
	mk(dev.ID, "cache", "redis:7.0.1", "manual", "semver") // mode 2, bare = ^7.0.1
	mk(prd.ID, "api", "nginx:1", "auto", "")               // promote rung: no direct release

	// Round 1: every image moved; one release for dev, none for prd.
	ups, err := f.Check(ctx, Scope{}, io.Discard)
	must(t, err)
	want := []string{"digest nginx:1", "tags redis", "digest redis:7.2.0"}
	if !slices.Equal(reg.calls, want) {
		t.Errorf("calls = %v, want %v", reg.calls, want)
	}
	if len(ups) != 1 || ups[0].EnvID != dev.ID || ups[0].Auto {
		t.Fatalf("updates = %+v, want one manual update for dev", ups)
	}
	pins, err := f.Releases.Pins(ctx, ups[0].ReleaseID)
	must(t, err)
	if pins["api"].Digest != "sha256:n1" || pins["web"].Repo != "nginx:1" || pins["cache"].Digest != "sha256:r72" {
		t.Errorf("pins = %+v", pins)
	}
	if img, _ := f.Images.GetByRef(ctx, "redis:7.0.1"); img.LastTag != "7.2.0" {
		t.Errorf("cached tag = %q", img.LastTag)
	}

	// Round 2: nothing moved, nothing written.
	if ups, err = f.Check(ctx, Scope{}, io.Discard); err != nil || len(ups) != 0 {
		t.Errorf("unchanged round: %v %+v", err, ups)
	}

	// Round 3: a registry error is never an update and keeps the last answer.
	reg.fail = map[string]bool{"nginx:1": true}
	reg.digests["redis:7.2.0"] = "sha256:r72" // unchanged
	if ups, err = f.Check(ctx, Scope{}, io.Discard); err != nil || len(ups) != 0 {
		t.Errorf("error round: %v %+v", err, ups)
	}
	if img, _ := f.Images.GetByRef(ctx, "nginx:1"); img.LastDigest != "sha256:n1" || img.LastError == "" {
		t.Errorf("after error: %+v", img)
	}

	// Round 4: nginx moves to what dev already runs: no release.
	reg.fail = nil
	reg.digests["nginx:1"] = "sha256:n2"
	cur, err := f.Releases.Create(ctx, stID, "test", []release.Pin{
		{Slug: "api", Repo: "nginx", Digest: "sha256:n2"},
		{Slug: "web", Repo: "nginx", Digest: "sha256:n2"},
	})
	must(t, err)
	dev.ReleaseID = &cur.ID
	must(t, s.Environments.Update(ctx, dev))
	if ups, err = f.Check(ctx, Scope{}, io.Discard); err != nil || len(ups) != 0 {
		t.Errorf("caught-up round: %v %+v", err, ups)
	}
	if img, _ := f.Images.GetByRef(ctx, "nginx:1"); !Chip(img, release.Pin{Digest: "sha256:n1"}) ||
		Chip(img, release.Pin{Digest: "sha256:n2"}) {
		t.Errorf("chip wrong for %+v", img)
	}

	// Timed sweep: the interval gates it.
	if due, _ := f.Due(ctx, now); !due {
		t.Error("first sweep not due")
	}
	if due, _ := f.Due(ctx, now.Add(time.Minute)); due {
		t.Error("due again inside the interval")
	}
}

func TestPick(t *testing.T) {
	tags := []string{"1.2.0", "1.2.5", "1.3.0", "v1.4.0-rc1", "2.0.0", "latest", "1.2"}
	for policy, want := range map[string]string{
		"semver ^1.2": "1.3.0",
		"semver ~1.2": "1.2.5",
		"semver 1":    "1.3.0",
		"semver *":    "2.0.0",
		"~1.2.1":      "1.2.5",
	} {
		if got, err := Pick(policy, "", tags); err != nil || got != want {
			t.Errorf("%s = %q %v, want %q", policy, got, err, want)
		}
	}
	if got, _ := Pick("semver", "1.2.0", tags); got != "1.3.0" {
		t.Errorf("bare semver = %q", got)
	}
	if _, err := Pick("semver ^9", "", tags); err == nil {
		t.Error("no match did not fail")
	}
}
