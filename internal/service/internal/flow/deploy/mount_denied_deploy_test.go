package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// A tile that mounts an NFS share and whose start the daemon refuses with
// "permission denied" fails the deploy with the plain NAS text and the raw
// daemon text, and the error still carries the typed original. The share tile
// stops before it starts, so this runs the stopFirst path through the fake.
func TestDeployShareMountDenied(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	fake := dockerfake.New()
	tiles := tile.New(st.Tiles, fake, nil)
	tiles.Gate = tile.Gate{Poll: time.Millisecond, Grace: time.Millisecond, Deadline: 10 * time.Millisecond}
	f := &Flow{
		LookupHost: func(c context.Context, _ string) ([]string, error) { return []string{"10.0.0.5"}, nil },
		Tiles:      tiles,
		Envs:       environment.New(st.Environments, fake),
		Stacks:     stack.New(st.Stacks),
		Volumes:    volume.New(st.Volumes, fake).WithShares(st.Shares),
		Images:     image.New(st.Images, fake),
		Params:     params.New(st.Params),
		Tiers:      tier.New(st.Tiers),
		Orgs:       org.New(st.Orgs, st.OrgMembers, st.Invites),
		Releases:   release.New(st.Releases, st.ReleaseTiles),
		Operated:   operated.New(st.OperatedInstances, st.Provisions, st.Bindings),
		Domains:    domain.New(st.Domains, fake, "proxy"),
		Cred:       credential.New(st.Credentials).For,
		Settings:   settings.New(st.Settings, nil),
		Jobs:       job.New(st.Jobs),
	}
	o := store.Org{ID: "o1", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}
	if err := st.Orgs.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	s := store.Stack{ID: "s1", OrgID: o.ID, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: time.Now()}
	if err := st.Stacks.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	e := store.Environment{ID: "e1", StackID: s.ID, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: time.Now()}
	if err := st.Environments.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Volumes.CreateShare(ctx, o.ID, volume.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas.lan:/export"}); err != nil {
		t.Fatal(err)
	}
	web, err := tiles.Create(ctx, store.Tile{StackID: s.ID, EnvironmentID: e.ID, Name: "web", Kind: tile.Image,
		ImageRef: "nginx:1", ContainerPort: 80, Volumes: "share:media:/data"})
	if err != nil {
		t.Fatal(err)
	}
	f.HostGrants = hostgrant.New(memGrants{s.ID: {ID: "g", StackID: s.ID, Lines: strings.Join([]string{
		hostgrant.Line("web", ShareGrant("media", "10.0.0.5")),
		hostgrant.Line("web", hostgrant.Source+tileSource(web)),
	}, "\n")}})
	fake.RunIDs = []string{"new"}
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: "healthy"}}

	// The share volume's name is the one the bind carries.
	bind, err := f.shareBind(ctx, o, e, s, web, mount(t, "share:media:/data"))
	if err != nil {
		t.Fatal(err)
	}
	vol, _, _ := strings.Cut(bind, ":")
	raw := "mounting " + vol + ": permission denied"
	fake.Err = map[string]error{"Start(new)": docker.MountDeniedError{Volume: vol, Raw: raw}}

	var log bytes.Buffer
	_, err = f.Run(ctx, web, "nginx@sha256:aa", &log, nil)
	if err == nil {
		t.Fatal("want the deploy to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "The NAS refused access to share media (nas.lan:/export)") || !strings.Contains(msg, raw) {
		t.Errorf("err = %q", msg)
	}
	var md docker.MountDeniedError
	if !errors.As(err, &md) || md.Volume != vol {
		t.Errorf("typed original lost: %v", err)
	}
	if !strings.Contains(log.String(), "The NAS refused access") {
		t.Errorf("log = %q", log.String())
	}
}
