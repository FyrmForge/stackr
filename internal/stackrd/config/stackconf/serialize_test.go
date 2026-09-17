package stackconf

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/stretchr/testify/require"
)

// TestStateToResolvedRoundTrip proves the phase-1 promise: serializing a live
// env back to config and diffing it against that same env yields no changes.
// This is what makes UI staging safe; a just-committed env shows an empty
// pending plan, and staged edits are the only diff.
func TestStateToResolvedRoundTrip(t *testing.T) {
	web := repo.Tile{
		ID: "web-id", Slug: "web", Kind: "service", SourceType: "git",
		GitURL: "https://github.com/org/web", GitBranch: "main",
		ConnectorID:  "cn-1",
		BuildContext: "apps/web", DockerfilePath: "Dockerfile.web",
		ContainerPort: 8080, HealthcheckCmd: "curl -f localhost", SecHeaders: true,
		Volumes: "cache:/cache", WatchPaths: "^apps/web/\n!\\.md$",
		CPULimit: 0.5, MemLimitMB: 256,
		Env:             "MODE=prod\nPORT=8080",
		BuildArgs:       "GO_VERSION=1.24",
		PublishedPorts:  "9000:9000\n5432:5432/udp",
		TraefikOverride: "http:\n  routers: {}",
		BasicAuthUser:   "admin", BasicAuthPassword: "${{ stack.secrets.PREVIEW_PASS }}",
	}
	api := repo.Tile{Slug: "api", Kind: "service", SourceType: "image", ImageRef: "nginx:1.27", ContainerPort: 80}
	cron := repo.Tile{Slug: "ping", Kind: "cron", SourceType: "image", ImageRef: "alpine:3", Cron: "0 3 * * *", Command: "sh /x.sh", TimeoutMinutes: 15}
	db := repo.Tile{Slug: "pg", Kind: "service", Engine: "postgres", CPULimit: 1, MemLimitMB: 512}
	vol := repo.Tile{ID: "vol-1", Slug: "webdata", Kind: "volume", AttachedTileID: "web-id", MountPath: "/data"}

	es := EnvState{Tiles: map[string]TileState{
		"web": {Tile: web, Domains: []repo.Domain{
			{Host: "web.example.com", Path: "/", HTTPS: true},
			{Host: "old.example.com", HTTPS: false, RedirectTo: "https://web.example.com"},
		}},
		"api":     {Tile: api},
		"ping":    {Tile: cron},
		"pg":      {Tile: db},
		"webdata": {Tile: vol},
	}}
	state := State{Envs: map[string]EnvState{"production": es}}

	// UI-managed stack: no bound git repo → no GitURL/DefaultBranch.
	r := StateToResolved("mystack", state)
	plan := Diff(r, state, DiffOpts{})
	require.True(t, plan.Empty(), "live env did not round-trip clean; spurious changes:\n%+v", plan.Changes)
}

// TestStateToResolvedDetectsEdit sanity-checks the other direction: a change to
// the desired config (as a staged edit would make) does show up as a diff.
func TestStateToResolvedDetectsEdit(t *testing.T) {
	tile := repo.Tile{Slug: "web", Kind: "service", SourceType: "image", ImageRef: "nginx:1", ContainerPort: 80}
	state := State{Envs: map[string]EnvState{"production": {Tiles: map[string]TileState{"web": {Tile: tile}}}}}

	r := StateToResolved("s", state)
	// stage an edit: change the port
	env := r.Envs["production"]
	tc := env.Tiles["web"]
	tc.Port = 9090
	env.Tiles["web"] = tc

	plan := Diff(r, state, DiffOpts{})
	require.False(t, plan.Empty(), "expected a change for the edited port, got clean plan")
	found := false
	for _, ch := range plan.Changes {
		if ch.Field == "port" && ch.New == "9090" {
			found = true
		}
	}
	require.True(t, found, "expected a port change to 9090, got %+v", plan.Changes)
}
