package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var (
	t0 = time.Date(2026, 9, 24, 10, 11, 12, 123456789, time.UTC)
	t1 = t0.Add(time.Hour)
)

func ptr[T any](v T) *T { return &v }

type crud[T any] interface {
	Create(ctx context.Context, row T) error
	Get(ctx context.Context, id string) (T, error)
	Update(ctx context.Context, row T) error
	Delete(ctx context.Context, id string) error
}

// roundTrip writes row, reads it back, changes every field mutate touches,
// writes and reads again. Every field is compared (B3).
func roundTrip[T any](t *testing.T, c crud[T], row T, mutate func(*T)) {
	t.Helper()
	ctx := context.Background()
	id := reflect.ValueOf(row).FieldByName("ID").String()
	if err := c.Create(ctx, row); err != nil {
		t.Fatalf("%T create: %v", row, err)
	}
	got, err := c.Get(ctx, id)
	if err != nil {
		t.Fatalf("%T get: %v", row, err)
	}
	same(t, "create", row, got)
	mutate(&row)
	if err := c.Update(ctx, row); err != nil {
		t.Fatalf("%T update: %v", row, err)
	}
	if got, err = c.Get(ctx, id); err != nil {
		t.Fatalf("%T get: %v", row, err)
	}
	same(t, "update", row, got)
}

// same compares field by field; times by Equal (the driver hands back its
// own location).
func same[T any](t *testing.T, step string, want, got T) {
	t.Helper()
	w, g := reflect.ValueOf(want), reflect.ValueOf(got)
	for i := range w.NumField() {
		if !w.Type().Field(i).IsExported() {
			continue
		}
		name := w.Type().Field(i).Name
		wf, gf := w.Field(i).Interface(), g.Field(i).Interface()
		switch wv := wf.(type) {
		case time.Time:
			if !wv.Equal(gf.(time.Time)) {
				t.Errorf("%T %s: %s = %v, want %v", want, step, name, gf, wv)
			}
		case *time.Time:
			gv := gf.(*time.Time)
			if (wv == nil) != (gv == nil) || (wv != nil && !wv.Equal(*gv)) {
				t.Errorf("%T %s: %s = %v, want %v", want, step, name, gv, wv)
			}
		default:
			if !reflect.DeepEqual(wf, gf) {
				t.Errorf("%T %s: %s = %#v, want %#v", want, step, name, gf, wf)
			}
		}
	}
}

func TestRoundTrip(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()

	roundTrip(t, s.Users, store.User{
		ID:           "u1",
		Email:        "a@x",
		PasswordHash: "h",
		Name:         "A",
		Role:         "user",
		Active:       true,
		AvatarPath:   "a.png",
		Theme:        "dark",
		CreatedAt:    t0,
		UpdatedAt:    t0,
	}, func(u *store.User) {
		u.Email = "b@x"
		u.Role = "admin"
		u.Active = false
		u.Theme = "light"
		u.UpdatedAt = t1
	})
	roundTrip(t, s.Orgs, store.Org{
		ID:                "o1",
		Name:              "Org",
		Slug:              "org",
		AvatarPath:        "o.png",
		EnvColors:         `{"prod":"red"}`,
		Settings:          "{}",
		SetupDoneAt:       nil,
		CreatedAt:         t0,
		ConfigConnectorID: "cn1",
		ConfigRepo:        "https://github.com/acme/infra",
		ConfigBranch:      "main",
		ConfigPath:        "stackr-org.yml",
	}, func(o *store.Org) {
		o.Name = "Org 2"
		o.SetupDoneAt = ptr(t1)
		o.ConfigBranch = "prod"
		o.ConfigAuto = true
	})
	roundTrip(t, s.OrgPlans, store.OrgPlan{
		ID:        "op1",
		OrgID:     "o1",
		Commit:    "abc123",
		Summary:   "1 to add",
		Plan:      `{"changes":[]}`,
		Status:    "pending",
		CreatedAt: t0,
	}, func(p *store.OrgPlan) {
		p.Status = "error"
		p.Error = "boom"
		p.DecidedAt = ptr(t1)
	})
	roundTrip(t, s.OrgMembers, store.OrgMember{
		ID:        "m1",
		OrgID:     "o1",
		UserID:    "u1",
		Role:      "owner",
		CreatedAt: t0,
	}, func(m *store.OrgMember) { m.Role = "member" })
	roundTrip(t, s.Invites, store.Invite{
		ID:        "i1",
		OrgID:     "o1",
		Email:     "c@x",
		Role:      "owner",
		CreatedBy: "u1",
		CreatedAt: t0,
		ExpiresAt: t1,
	}, func(i *store.Invite) { i.UsedAt = ptr(t1) })
	roundTrip(t, s.APIKeys, store.APIKey{
		ID:        "k1",
		UserID:    "u1",
		OrgID:     ptr("o1"),
		Name:      "ci",
		TokenHash: "th",
		CreatedAt: t0,
	}, func(k *store.APIKey) { k.Name, k.OrgID = "ci2", nil })
	roundTrip(t, s.Stacks, store.Stack{
		ID:                "s1",
		OrgID:             "o1",
		Name:              "S",
		Slug:              "s",
		Description:       "d",
		Settings:          "{}",
		ConfigConnectorID: "c",
		ConfigRepo:        "r",
		ConfigBranch:      "main",
		ConfigPath:        "stackr.yml",
		CreatedAt:         t0,
	}, func(st *store.Stack) { st.Description, st.ConfigBranch = "d2", "dev" })
	roundTrip(t, s.Releases, store.Release{
		ID:        "r1",
		StackID:   "s1",
		Number:    1,
		CreatedAt: t0,
		CreatedBy: "u1",
	}, func(r *store.Release) { r.Number = 2 })
	roundTrip(t, s.Environments, store.Environment{
		ID:         "e1",
		StackID:    "s1",
		Name:       "Prod",
		Slug:       "prod",
		Type:       "static",
		Settings:   "{}",
		Color:      "red",
		Position:   1,
		Network:    "net1",
		FromKind:   "branch",
		FromBranch: "main",
		Auto:       true,
		CreatedAt:  t0,
	}, func(e *store.Environment) {
		e.ReleaseID = ptr("r1")
		e.FromKind = "promote"
		e.Auto = false
		e.Position = 2
	})
	roundTrip(t, s.Environments, store.Environment{
		ID:         "e2",
		StackID:    "s1",
		Name:       "PR 7",
		Slug:       "pr-7",
		Type:       "ephemeral",
		BaseEnvID:  ptr("e1"),
		Settings:   "{}",
		Color:      "",
		Position:   3,
		Network:    "net2",
		FromKind:   "branch",
		FromBranch: "pr-7",
		CreatedAt:  t0,
	}, func(e *store.Environment) { e.BaseEnvID = nil })
	roundTrip(t, s.Tiles, store.Tile{
		ID:                      "t1",
		StackID:                 "s1",
		EnvironmentID:           "e1",
		Name:                    "Web",
		Slug:                    "web",
		Kind:                    "service",
		GitURL:                  "https://g/x",
		GitBranch:               "main",
		ImageRef:                "",
		DockerfilePath:          "Dockerfile",
		BuildContext:            ".",
		WatchPaths:              "src/**",
		EnvJSON:                 `{"A":"1"}`,
		BuildArgs:               "{}",
		Volumes:                 "data:/data",
		Command:                 "run",
		ContainerPort:           8080,
		PublishedPorts:          "80:8080",
		EndpointProtocol:        "http",
		HealthPath:              "/health",
		HealthcheckCmd:          "true",
		HealthcheckIntervalS:    10,
		HealthcheckTimeoutS:     5,
		HealthcheckRetries:      3,
		HealthcheckStartPeriodS: 15,
		CPULimit:                1.5,
		MemLimitMB:              512,
		User:                    "1000",
		ShmSizeMB:               64,
		Privileged:              true,
		Devices:                 "/dev/x",
		RestartPolicy:           "always",
		DependsOn:               "db",
		Files:                   "{}",
		SharedNet:               "shared",
		Replicas:                2,
		UpdatePolicy:            "auto",
		TagPolicy:               "semver",
		CreatedAt:               t0,
		UpdatedAt:               t0,
	}, func(ti *store.Tile) {
		ti.Kind = "image"
		ti.CPULimit = 0.25
		ti.Privileged = false
		ti.UpdatePolicy = "manual"
		ti.SliceAccess = store.SliceAccessList{
			{
				From:   "api-db",
				Access: "read",
			},
		}
		ti.UpdatedAt = t1
	})
	roundTrip(t, s.Tiles, store.Tile{
		ID:            "t2",
		StackID:       "s1",
		EnvironmentID: "e1",
		Name:          "DB",
		Slug:          "db",
		Kind:          "managed",
		UpdatePolicy:  "manual",
		CreatedAt:     t0,
		UpdatedAt:     t0,
	}, func(ti *store.Tile) { ti.Replicas = 1 })
	roundTrip(t, s.Tiles, store.Tile{
		ID:            "t3",
		StackID:       "s1",
		EnvironmentID: "e1",
		Name:          "api-db",
		Slug:          "api-db",
		Kind:          "slice",
		UpdatePolicy:  "manual",
		ProvisionFrom: ptr("infra:${{ env.name }}:pg_db"),
		DefaultAccess: ptr("write"),
		CreatedAt:     t0,
		UpdatedAt:     t0,
	}, func(ti *store.Tile) {
		ti.ProvisionFrom = ptr("infra:staging:pg_db")
		ti.DefaultAccess = ptr("read")
	})
	roundTrip(t, s.Images, store.Image{
		ID:         "img1",
		Ref:        "reg/x:1",
		Digest:     "sha256:a",
		BuiltAt:    ptr(t0),
		LastDigest: "sha256:b",
		LastTag:    "1",
		LastError:  "",
		CheckedAt:  nil,
		CreatedAt:  t0,
	}, func(i *store.Image) { i.LastError, i.CheckedAt, i.BuiltAt = "timeout", ptr(t1), nil })
	roundTrip(t, s.ReleaseTiles, store.ReleaseTile{
		ID:        "rt1",
		ReleaseID: "r1",
		Slug:      "web",
		Repo:      "g/x",
		Branch:    "main",
		CommitSHA: "abc",
		ImageID:   ptr("img1"),
		Digest:    "sha256:a",
	}, func(r *store.ReleaseTile) { r.ImageID, r.CommitSHA = nil, "def" })
	roundTrip(t, s.Params, store.Param{
		ID:         "p1",
		ScopeKind:  "env",
		ScopeID:    "e1",
		Collection: "db",
		Name:       "password",
		Kind:       "secret",
		Value:      "hunter2",
		CreatedAt:  t0,
		UpdatedAt:  t0,
	}, func(p *store.Param) { p.Value, p.UpdatedAt = "hunter3", t1 })
	roundTrip(t, s.ManagedInstances, store.ManagedInstance{
		ID:     "mi1",
		TileID: "t2",
		Engine: "postgres",
		Allow: store.StringList{
			"org:shop:*",
			"org:blog:*:api",
		},
		EnvPairs: store.StringMap{
			"dev":     "staging",
			"staging": "staging",
		},
		AdminUser:     "root",
		AdminPassword: "pw",
		Endpoint:      "db:5432",
		CreatedAt:     t0,
	}, func(m *store.ManagedInstance) {
		m.AdminPassword = "pw2"
		m.Allow = store.StringList{}
		m.EnvPairs = store.StringMap{}
	})
	roundTrip(t, s.Provisions, store.Provision{
		ID:         "pr1",
		TileID:     "t3",
		InstanceID: "mi1",
		DBName:     "api_db",
		DBUser:     "api_db",
		DBPassword: "pw",
		Public:     true,
		OnRemove:   "keep",
		CreatedAt:  t0,
	}, func(p *store.Provision) {
		p.DBPassword = "pw2"
		p.Public = false
		p.OnRemove = "drop"
	})
	if p, err := s.Provisions.GetByTile(ctx, "t3"); err != nil || p.ID != "pr1" {
		t.Fatalf("provision by slice tile = %+v, %v", p, err)
	}
	roundTrip(t, s.Bindings, store.Binding{
		ID:             "b1",
		ProvisionID:    "pr1",
		ConsumerTileID: "t1",
		Access:         "write",
		DBUser:         "api_db_web",
		DBPassword:     "pw",
		Outputs:        `{"DATABASE_URL":"postgres://x"}`,
		CreatedAt:      t0,
	}, func(b *store.Binding) {
		b.Access = "read"
		b.DBPassword = "pw2"
		b.Outputs = "{}"
	})
	if bs, err := s.Bindings.ListByProvision(ctx, "pr1"); err != nil || len(bs) != 1 || bs[0].ID != "b1" {
		t.Fatalf("bindings by provision = %+v, %v", bs, err)
	}
	if bs, err := s.Bindings.ListByConsumer(ctx, "t1"); err != nil || len(bs) != 1 || bs[0].ID != "b1" {
		t.Fatalf("bindings by consumer = %+v, %v", bs, err)
	}
	roundTrip(t, s.Volumes, store.Volume{
		ID:         "v1",
		ScopeKind:  "env",
		ScopeID:    "e1",
		InstanceID: ptr("mi1"),
		Slug:       "data",
		Name:       "stackr_data",
		MaxSizeMB:  100,
		CreatedAt:  t0,
	}, func(v *store.Volume) { v.InstanceID, v.OrphanedAt = nil, ptr(t1) })
	roundTrip(t, s.DomainResources, store.DomainResource{
		ID:        "dr1",
		Level:     "instance",
		Host:      "example.com",
		CreatedAt: t0,
	}, func(r *store.DomainResource) { r.ACMEEmail, r.IncludeEnvOnDefault = "ops@example.com", true })
	roundTrip(t, s.DomainResources, store.DomainResource{
		ID:        "dr2",
		Level:     "org",
		OrgID:     ptr("o1"),
		Host:      "org.example.com",
		Declared:  true,
		CreatedAt: t0,
	}, func(r *store.DomainResource) { r.Declared = false })
	roundTrip(t, s.DomainResources, store.DomainResource{
		ID:        "dr3",
		Level:     "stack",
		StackID:   ptr("s1"),
		Host:      "shop.io",
		ACMEEmail: "a@shop.io",
		Declared:  true,
		CreatedAt: t0,
	}, func(r *store.DomainResource) { r.ACMEEmail = "" })
	resources, err := s.DomainResources.List(ctx)
	if err != nil || len(resources) != 3 {
		t.Fatalf("domain resources = %d, %v; want 3", len(resources), err)
	}
	roundTrip(t, s.Domains, store.Domain{
		ID:            "d1",
		TileID:        "t1",
		Host:          "x.io",
		Path:          "/",
		ContainerPort: 8080,
		HTTPS:         true,
		ForceHTTPS:    true,
		RedirectTo:    "",
		Auto:          true,
		ResourceID:    ptr("dr3"),
		Position:      1,
		ProxyJSON:     "{}",
		RawCaddy:      "",
		CreatedAt:     t0,
	}, func(d *store.Domain) {
		d.RedirectTo, d.HTTPS, d.RawCaddy = "y.io", false, "header X 1"
		d.ResourceID = ptr("dr2")
	})
	roundTrip(t, s.Credentials, store.Credential{
		ID:        "c1",
		OrgID:     "o1",
		Name:      "ghcr",
		URL:       "ghcr.io",
		Username:  "me",
		Password:  "tok",
		CreatedAt: t0,
	}, func(c *store.Credential) { c.Password = "tok2" })
	roundTrip(t, s.Connectors, store.Connector{
		ID:        "cn1",
		OrgID:     "o1",
		Provider:  "github",
		Name:      "gh",
		Host:      "github.com",
		Config:    `{"key":"PEM"}`,
		CreatedAt: t0,
	}, func(c *store.Connector) { c.Config = "{}" })
	roundTrip(t, s.BackupDests, store.BackupDest{
		ID:         "bd1",
		OrgID:      ptr("o1"),
		Kind:       "s3",
		Name:       "b2",
		Endpoint:   "e",
		Region:     "r",
		Bucket:     "b",
		AccessKey:  "ak",
		SecretKey:  "sk",
		ArchiveKey: "age1",
		Shared:     true,
		CreatedAt:  t0,
	}, func(d *store.BackupDest) { d.OrgID, d.SecretKey, d.Shared = nil, "sk2", false })
	roundTrip(t, s.BackupSchedules, store.BackupSchedule{
		ID:        "bs1",
		VolumeID:  "v1",
		Method:    "tar",
		DestID:    ptr("bd1"),
		Cron:      "0 3 * * *",
		Timezone:  "UTC",
		Keep:      7,
		Mode:      "hot",
		CreatedAt: t0,
	}, func(b *store.BackupSchedule) { b.DestID, b.Keep = nil, 3 })
	roundTrip(t, s.BackupRuns, store.BackupRun{
		ID:         "br1",
		Kind:       "volume",
		VolumeID:   ptr("v1"),
		ScheduleID: ptr("bs1"),
		DestID:     "bd1",
		Trigger:    "cron",
		Status:     "running",
		ObjectKey:  "k",
		SizeBytes:  1 << 40,
		CreatedAt:  t0,
	}, func(b *store.BackupRun) { b.Status, b.FinishedAt, b.VolumeID = "done", ptr(t1), nil })
	roundTrip(t, s.Jobs, store.Job{
		ID:        "j1",
		Kind:      "deploy",
		State:     "queued",
		ReleaseID: ptr("r1"),
		LockSet:   store.StringList{"t1", "t2"},
		Payload:   `{"x":1}`,
		LogPath:   "/j1.log",
		CreatedAt: t0,
	}, func(j *store.Job) {
		j.State = "waiting"
		j.WaitingParam = ptr("db/password")
		j.StartedAt = ptr(t1)
		j.LockSet = store.StringList{}
	})

	// Settings and sessions have no id-keyed CRUD.
	if _, ok, err := s.Settings.Get(ctx, "workers"); ok || err != nil {
		t.Fatalf("unset key: ok=%v err=%v", ok, err)
	}
	for _, v := range []string{"2", "4"} {
		if err := s.Settings.Set(ctx, "workers", v); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := s.Settings.Get(ctx, "workers"); got != v || !ok || err != nil {
			t.Fatalf("settings = %q %v %v, want %q", got, ok, err, v)
		}
	}
	se := &auth.Session{
		ID:        "se1",
		SubjectID: "u1",
		Token:     "tok",
		ExpiresAt: t1,
		CreatedAt: t0,
	}
	if err := s.Sessions.Create(ctx, se); err != nil {
		t.Fatal(err)
	}
	got, err := s.Sessions.GetByToken(ctx, "tok")
	if err != nil || got == nil {
		t.Fatalf("session: %v %v", got, err)
	}
	same(t, "create", *se, *got)
	if got, err := s.Sessions.GetByToken(ctx, "nope"); got != nil || err != nil {
		t.Fatalf("missing token = %v, %v; hamr wants nil, nil", got, err)
	}

	// Deletes, children first, so each proves itself rather than a cascade.
	for _, d := range []struct {
		name string
		del  func(context.Context, string) error
		id   string
	}{
		{"jobs", s.Jobs.Delete, "j1"},
		{"org_config_plans", s.OrgPlans.Delete, "op1"},
		{"backup_runs", s.BackupRuns.Delete, "br1"},
		{"backup_schedules", s.BackupSchedules.Delete, "bs1"},
		{"backup_destinations", s.BackupDests.Delete, "bd1"},
		{"connectors", s.Connectors.Delete, "cn1"},
		{"credentials", s.Credentials.Delete, "c1"},
		{"domains", s.Domains.Delete, "d1"},
		{"domain_resources", s.DomainResources.Delete, "dr2"},
		{"volumes", s.Volumes.Delete, "v1"},
		{"bindings", s.Bindings.Delete, "b1"},
		{"provisions", s.Provisions.Delete, "pr1"},
		{"managed_instances", s.ManagedInstances.Delete, "mi1"},
		{"params", s.Params.Delete, "p1"},
		{"release_tiles", s.ReleaseTiles.Delete, "rt1"},
		{"images", s.Images.Delete, "img1"},
		{"tiles", s.Tiles.Delete, "t2"},
		{"environments", s.Environments.Delete, "e2"},
		{"releases", s.Releases.Delete, "r1"},
		{"api_keys", s.APIKeys.Delete, "k1"},
		{"invites", s.Invites.Delete, "i1"},
		{"org_members", s.OrgMembers.Delete, "m1"},
	} {
		if err := d.del(ctx, d.id); err != nil {
			t.Errorf("%s delete: %v", d.name, err)
		}
		if err := d.del(ctx, d.id); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s second delete: %v, want ErrNotFound", d.name, err)
		}
	}
}

func TestEncryptedAtRest(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	mustSeedOrg(t, s)
	if err := s.Credentials.Create(ctx, store.Credential{
		ID:        "c1",
		OrgID:     "o1",
		Name:      "n",
		Password:  "hunter2",
		CreatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.DB().Get(&raw, `SELECT password FROM credentials WHERE id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "enc1:") || strings.Contains(raw, "hunter2") {
		t.Errorf("stored password = %q, want ciphertext", raw)
	}
}

func TestErrors(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	if _, err := s.Orgs.Get(ctx, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	if err := s.Orgs.Update(ctx, store.Org{ID: "nope"}); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	mustSeedOrg(t, s)
	err := s.Orgs.Create(ctx, store.Org{ID: "o2", Slug: "org", CreatedAt: t0})
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("duplicate slug: %v, want Conflict", err)
	}
}

func TestTxRollsBack(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Orgs.Create(ctx, store.Org{ID: "o1", Slug: "a", CreatedAt: t0}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("tx err = %v", err)
	}
	if _, err := s.Orgs.Get(ctx, "o1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("rolled-back row is there: %v", err)
	}
	if err := s.Tx(ctx, func(tx store.Tx) error {
		return tx.Orgs.Create(ctx, store.Org{ID: "o1", Slug: "a", CreatedAt: t0})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orgs.Get(ctx, "o1"); err != nil {
		t.Errorf("committed row missing: %v", err)
	}
}

func mustSeedOrg(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.Orgs.Create(context.Background(), store.Org{
		ID:        "o1",
		Name:      "Org",
		Slug:      "org",
		CreatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
}
