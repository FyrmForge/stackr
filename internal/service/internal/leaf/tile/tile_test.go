package tile_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

type vipStub struct{ set map[string][]string }

func (v *vipStub) Set(_ context.Context, ip string, rs []string) error {
	v.set[ip] = rs
	return nil
}

func (v *vipStub) Remove(_ context.Context, ip string) error {
	delete(v.set, ip)
	return nil
}

// seed makes an org, stack and env and returns the env's ids.
func seed(t *testing.T, st *store.Store) (stackID, envID string) {
	t.Helper()
	org, stackID, envID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        org,
		Name:      org,
		Slug:      org[:8],
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        stackID,
		OrgID:     org,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		Domains:   "[]",
		CreatedAt: now,
	}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID:         envID,
		StackID:    stackID,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	return stackID, envID
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (*tile.Leaf, *dockerfake.Fake, *vipStub, store.Tile) {
	st := servicetest.Store(t)
	fake, v := dockerfake.New(), &vipStub{set: map[string][]string{}}
	l := tile.New(st.Tiles, fake, v)
	l.Gate = tile.Gate{Poll: time.Millisecond, Grace: 20 * time.Millisecond, Deadline: 60 * time.Millisecond}
	s, e := seed(t, st)
	return l, fake, v, store.Tile{StackID: s, EnvironmentID: e}
}

func svc(base store.Tile, name string) store.Tile {
	base.Name, base.Kind, base.GitURL = name, tile.Service, "https://github.com/acme/api"
	return base
}

func TestCreateRename(t *testing.T) {
	l, _, _, base := setup(t)
	api, err := l.Create(ctx, svc(base, "API Server"))
	if err != nil || api.Slug != "api-server" || api.GitBranch != "main" || api.Replicas != 1 ||
		api.RestartPolicy != "always" || api.UpdatePolicy != "manual" || api.EnvJSON != "{}" {
		t.Fatalf("create = %+v, %v", api, err)
	}
	if _, err := l.Create(ctx, svc(base, "api server")); !isConflict(err) {
		t.Errorf("same slug in the env = %v, want conflict", err)
	}
	for _, name := range []string{"", "Params", "!!"} {
		if _, err := l.Create(ctx, svc(base, name)); err == nil {
			t.Errorf("create %q accepted", name)
		}
	}
	db, _ := l.Create(ctx, svc(base, "db"))
	if _, err := l.Rename(ctx, db, "API server"); !isConflict(err) {
		t.Errorf("rename onto a taken slug = %v", err)
	}
	if _, err := l.Rename(ctx, db, ""); err == nil {
		t.Error("rename to empty accepted")
	}
	if db, err = l.Rename(ctx, db, "Orders DB"); err != nil || db.Slug != "orders-db" {
		t.Errorf("rename = %+v, %v", db, err)
	}
}

func isConflict(err error) bool {
	_, ok := errs.IsConflict(err)
	return ok
}

func field(err error) string {
	v, _ := errs.IsInvalid(err)
	return v.Field
}

// B26: one whitelist of what each kind may carry.
func TestKindWhitelist(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(*store.Tile)
		field string
	}{
		{"service ok", func(*store.Tile) {}, ""},
		{"service without git", func(t *store.Tile) { t.GitURL = "" }, "git_url"},
		{"service with a gitlab url", func(t *store.Tile) { t.GitURL = "https://gitlab.com/a/b" }, "git_url"},
		{"service with an image", func(t *store.Tile) { t.ImageRef = "nginx:1" }, "image"},
		{"service on auto update", func(t *store.Tile) { t.UpdatePolicy = "auto" }, "update_policy"},
		{"image ok", func(t *store.Tile) {
			t.Kind = tile.Image
			t.GitURL = ""
			t.ImageRef = "nginx:1"
			t.UpdatePolicy = "auto"
		}, ""},
		{"image without an image", func(t *store.Tile) { t.Kind, t.GitURL = tile.Image, "" }, "image"},
		{"image with a dockerfile", func(t *store.Tile) {
			t.Kind = tile.Image
			t.GitURL = ""
			t.ImageRef = "nginx:1"
			t.DockerfilePath = "D"
		}, "dockerfile"},
		{"managed ok", func(t *store.Tile) { t.Kind, t.GitURL, t.MemLimitMB = tile.Managed, "", 512 }, ""},
		{"managed with a port", func(t *store.Tile) {
			t.Kind, t.GitURL, t.ContainerPort = tile.Managed, "", 5432
		}, "port"},
		{"managed with replicas", func(t *store.Tile) {
			t.Kind, t.GitURL, t.Replicas = tile.Managed, "", 2
		}, "replicas"},
		{"unknown kind", func(t *store.Tile) { t.Kind = "batch" }, "kind"},
		{"bad env key", func(t *store.Tile) { t.EnvJSON = `{"1BAD":"x"}` }, "env"},
		{"bad restart", func(t *store.Tile) { t.RestartPolicy = "sometimes" }, "restart"},
		{"negative cpu", func(t *store.Tile) { t.CPULimit = -1 }, "limits.cpu"},
		{"bad device", func(t *store.Tile) { t.Devices = "dev/x" }, "devices"},
		{"bad dep", func(t *store.Tile) { t.DependsOn = "db:ready" }, "depends_on"},
		{"file outside the repo", func(t *store.Tile) { t.Files = "../x:/etc/x" }, "files"},
		{"replicas with a mount", func(t *store.Tile) { t.Volumes, t.Replicas = "data:/data", 2 }, "replicas"},
		{"replicas without a mount", func(t *store.Tile) { t.Replicas = 3 }, ""},
	} {
		row := store.Tile{Name: "x", Kind: tile.Service, GitURL: "https://github.com/a/b"}
		c.edit(&row)
		err := tile.Validate(&row)
		if got := field(err); got != c.field || (c.field == "" && err != nil) {
			t.Errorf("%s: err = %v (field %q), want field %q", c.name, err, got, c.field)
		}
	}
}

// B26 for the run-to-completion kinds, refusals worded as tilelifecycle has them.
func TestRunKinds(t *testing.T) {
	cronRow := func(t *store.Tile) { t.Kind, t.Schedule, t.Command = tile.Cron, "*/5 * * * *", "./sweep" }
	fnRow := func(t *store.Tile) { t.Kind, t.GitURL, t.ImageRef = tile.Function, "", "alpine:3" }
	then := func(a, b func(*store.Tile)) func(*store.Tile) {
		return func(t *store.Tile) {
			a(t)
			b(t)
		}
	}
	for _, c := range []struct {
		name, msg string
		edit      func(*store.Tile)
	}{
		{
			"cron ok",
			"",
			cronRow,
		},
		{
			"cron with CRON_TZ",
			"",
			then(cronRow, func(t *store.Tile) { t.Schedule = "CRON_TZ=Europe/London 0 3 * * *" }),
		},
		{
			"cron paused",
			"",
			then(cronRow, func(t *store.Tile) { t.Paused = true }),
		},
		{
			"cron from an image",
			"",
			then(cronRow, func(t *store.Tile) { t.GitURL, t.ImageRef = "", "alpine:3" }),
		},
		{
			"cron without a schedule",
			"a cron needs a schedule",
			then(cronRow, func(t *store.Tile) { t.Schedule = "" }),
		},
		{
			"cron with a bad schedule",
			"expected exactly 5 fields, found 2: [every day]",
			then(cronRow, func(t *store.Tile) { t.Schedule = "every day" }),
		},
		{
			"cron with a port",
			"a cron has no endpoint; port does not apply",
			then(cronRow, func(t *store.Tile) { t.ContainerPort = 80 }),
		},
		{
			"cron with replicas",
			"cron tiles do not take replicas",
			then(cronRow, func(t *store.Tile) { t.Replicas = 2 }),
		},
		{
			"cron with a healthcheck",
			"healthcheck applies to service tiles only",
			then(cronRow, func(t *store.Tile) { t.HealthcheckCmd = "true" }),
		},
		{
			"cron with a user",
			"user applies to service tiles only",
			then(cronRow, func(t *store.Tile) { t.User = "1000" }),
		},
		{
			"cron with devices",
			"devices apply to service tiles only",
			then(cronRow, func(t *store.Tile) { t.Devices = "/dev/x" }),
		},
		{
			"cron with a trigger",
			"run_on_deploy applies to function tiles only",
			then(cronRow, func(t *store.Tile) { t.Trigger = "on_deploy" }),
		},
		{
			"cron with git and an image",
			"a cron builds from git_url or runs an image, not both",
			then(cronRow, func(t *store.Tile) { t.ImageRef = "alpine:3" }),
		},
		{
			"cron with neither source",
			"a cron needs a git_url or an image",
			then(cronRow, func(t *store.Tile) { t.GitURL = "" }),
		},
		{
			"negative timeout",
			"timeout_minutes must not be negative",
			then(cronRow, func(t *store.Tile) { t.TimeoutMinutes = -1 }),
		},
		{
			"function ok",
			"",
			fnRow,
		},
		{
			"function on deploy",
			"",
			then(fnRow, func(t *store.Tile) { t.Trigger = "on_deploy" }),
		},
		{
			"function with a bad trigger",
			"trigger must be manual or on_deploy",
			then(fnRow, func(t *store.Tile) { t.Trigger = "hourly" }),
		},
		{
			"function with a schedule",
			"schedule applies to cron tiles only",
			then(fnRow, func(t *store.Tile) { t.Schedule = "* * * * *" }),
		},
		{
			"function paused",
			"only cron tiles have a schedule to pause",
			then(fnRow, func(t *store.Tile) { t.Paused = true }),
		},
		{
			"function with a port",
			"a function has no endpoint; port does not apply",
			then(fnRow, func(t *store.Tile) { t.ContainerPort = 80 }),
		},
		{
			"function with privileged",
			"privileged applies to service tiles only",
			then(fnRow, func(t *store.Tile) { t.Privileged = true }),
		},
		{
			"service with a schedule",
			"schedule applies to cron tiles only",
			func(t *store.Tile) { t.Schedule = "* * * * *" },
		},
		{
			"service with a trigger",
			"run_on_deploy applies to function tiles only",
			func(t *store.Tile) { t.Trigger = "manual" },
		},
		{
			"service with a timeout",
			"service tiles do not take timeout_minutes",
			func(t *store.Tile) { t.TimeoutMinutes = 5 },
		},
		{
			"managed with a command",
			"command does not apply to a managed",
			func(t *store.Tile) { t.Kind, t.GitURL, t.Command = tile.Managed, "", "x" },
		},
	} {
		row := store.Tile{Name: "x", Kind: tile.Service, GitURL: "https://github.com/a/b"}
		c.edit(&row)
		err := tile.Validate(&row)
		v, _ := errs.IsInvalid(err)
		if got := v.Msg; got != c.msg || (c.msg == "" && err != nil) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.msg)
		}
		if err == nil && tile.RunToCompletion(row.Kind) && row.TimeoutMinutes != tile.DefaultTimeout {
			t.Errorf("%s: timeout = %d, want the default", c.name, row.TimeoutMinutes)
		}
		if err == nil && row.Kind == tile.Function && row.Trigger == "" {
			t.Errorf("%s: trigger not defaulted", c.name)
		}
	}
}

// Which keys moved decides the effect.
func TestSideEffects(t *testing.T) {
	l, _, _, base := setup(t)
	api, err := l.Create(ctx, svc(base, "api"))
	must(t, err)
	for _, c := range []struct {
		name  string
		edit  func(*store.Tile)
		extra []string
		want  []tile.Effect
	}{
		{"nothing", func(*store.Tile) {}, nil, nil},
		{"restart spelling", func(t *store.Tile) { t.RestartPolicy = "" }, nil, nil},
		{"watch paths only", func(t *store.Tile) { t.WatchPaths = "src/**" }, nil, nil},
		{"domain only", func(*store.Tile) {}, []string{"domain +api.x.io"}, []tile.Effect{tile.Route}},
		{"port", func(t *store.Tile) { t.ContainerPort = 8080 }, nil, []tile.Effect{tile.Route, tile.Redeploy}},
		{"healthcheck", func(t *store.Tile) {
			t.HealthcheckRetries = 3
		}, nil, []tile.Effect{tile.Route, tile.Redeploy}},
		{"slug is identity", func(t *store.Tile) { t.Slug = "other" }, nil, nil},
	} {
		cur := api
		c.edit(&cur)
		got, effects, err := l.Update(ctx, api, cur, c.extra...)
		if err != nil || !slices.Equal(effects, c.want) || got.Slug != "api" {
			t.Errorf("%s: effects %v, slug %s, err %v; want %v", c.name, effects, got.Slug, err, c.want)
		}
	}
	if !(tile.Changed{"branch": true}).NeedsBuild() || (tile.Changed{"port": true}).NeedsBuild() {
		t.Error("NeedsBuild")
	}
	if e := tile.Effects(tile.Managed, tile.Changed{"limits": true}); !slices.Equal(e, []tile.Effect{tile.Redeploy}) {
		t.Errorf("managed limits = %v", e)
	}
	for _, c := range []struct {
		kind string
		c    tile.Changed
		want []tile.Effect
	}{
		{tile.Cron, tile.Changed{"schedule": true}, []tile.Effect{tile.CronReload}},
		{tile.Cron, tile.Changed{"paused": true}, []tile.Effect{tile.CronReload}},
		{tile.Cron, tile.Changed{"command": true, "timeout_minutes": true}, nil},
		{tile.Cron, tile.Changed{"git_url": true}, []tile.Effect{tile.Redeploy}},
		{tile.Function, tile.Changed{"trigger": true, "env": true}, nil},
		{tile.Function, tile.Changed{"branch": true}, []tile.Effect{tile.Redeploy}},
	} {
		if e := tile.Effects(c.kind, c.c); !slices.Equal(e, c.want) {
			t.Errorf("%s %v = %v, want %v", c.kind, c.c, e, c.want)
		}
	}
}

// The gate: healthy passes, a restart fails, a stuck "starting" times out,
// no HEALTHCHECK passes after the grace period. A failed replica is removed.
func TestGate(t *testing.T) {
	for _, c := range []struct {
		name string
		d    docker.Detail
		ok   bool
	}{
		{"healthy", docker.Detail{Running: true, Health: "healthy"}, true},
		{"no healthcheck, past grace", docker.Detail{Running: true}, true},
		{"crash loop", docker.Detail{Running: true, Health: "healthy", RestartCount: 1}, false},
		{"exited", docker.Detail{Health: "starting"}, false},
		{"unhealthy", docker.Detail{Running: true, Health: "unhealthy"}, false},
		{"timeout", docker.Detail{Running: true, Health: "starting"}, false},
	} {
		l, fake, _, base := setup(t)
		fake.RunID = "c1"
		fake.Details = map[string]docker.Detail{"c1": c.d}
		id, err := l.Start(ctx, svc(base, "api"), docker.ContainerSpec{Name: "api-1"})
		removed := slices.ContainsFunc(fake.Calls(), func(k dockerfake.Call) bool { return k.Method == "StopRemove" })
		if (err == nil) != c.ok || removed == c.ok || (c.ok && id != "c1") {
			t.Errorf("%s: id %q, err %v, removed %v", c.name, id, err, removed)
		}
	}
}

// One guard over stop, restart, remove and the terminal.
func TestGuard(t *testing.T) {
	l, fake, _, _ := setup(t)
	fake.Containers = []docker.Container{
		{ID: "panel", Labels: map[string]string{tile.LabelSystem: "true"}},
		{ID: "mine", Labels: map[string]string{tile.LabelTile: "t1"}},
		{ID: "theirs", Labels: map[string]string{tile.LabelTile: "t2"}},
	}
	fake.Details = map[string]docker.Detail{"panel": {Running: true}}
	if err := l.Stop(ctx, "", "panel"); !isConflict(err) {
		t.Errorf("stop panel = %v", err)
	}
	if _, err := l.Exec(ctx, "", "panel", []string{"sh"}); !isConflict(err) {
		t.Errorf("exec panel = %v", err)
	}
	if err := l.Remove(ctx, "", "panel"); !isConflict(err) {
		t.Errorf("remove running panel = %v", err)
	}
	fake.Details = map[string]docker.Detail{"panel": {Running: false}}
	if err := l.Remove(ctx, "", "panel"); err != nil {
		t.Errorf("remove exited panel = %v", err)
	}
	if err := l.Restart(ctx, "t1", "theirs"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("another tile's container = %v", err)
	}
	if err := l.Restart(ctx, "t1", "mine"); err != nil {
		t.Errorf("own container: %v", err)
	}
	fake.Err = map[string]error{"List": errors.New("daemon down")}
	if err := l.Stop(ctx, "t1", "mine"); !isConflict(err) {
		t.Errorf("list failure = %v, want refused (fail closed)", err)
	}
}

func TestRouteAndTeardown(t *testing.T) {
	l, fake, v, base := setup(t)
	api := svc(base, "api")
	api.ID = "t1"
	fake.Containers = []docker.Container{
		{ID: "p", Labels: map[string]string{tile.LabelTile: "t1", tile.LabelRole: "pause"}},
		{ID: "r1", Labels: map[string]string{tile.LabelTile: "t1", tile.LabelRole: "replica"}},
		{ID: "r2", Labels: map[string]string{tile.LabelTile: "t1", tile.LabelRole: "replica"}},
	}
	fake.Details = map[string]docker.Detail{
		"p":  {Networks: map[string]string{"n": "10.0.0.2"}},
		"r1": {Running: true, Networks: map[string]string{"n": "10.0.0.3"}},
		"r2": {Networks: map[string]string{"n": "10.0.0.4"}},
	}
	must(t, l.Route(ctx, api, "n"))
	if got := v.set["10.0.0.2"]; !slices.Equal(got, []string{"10.0.0.3"}) {
		t.Errorf("vip = %v, want only the running replica", got)
	}
	must(t, l.Teardown(ctx, api, "n"))
	if len(v.set) != 0 {
		t.Errorf("vip left: %v", v.set)
	}
}
