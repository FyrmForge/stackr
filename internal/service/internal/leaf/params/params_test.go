package params_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

// Scope ids need no parent rows: params carry no foreign key (triggers clean up).
var (
	org   = params.Scope{Kind: "org", ID: "o1"}
	stack = params.Scope{Kind: "stack", ID: "s1"}
	env   = params.Scope{Kind: "env", ID: "e1"}
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// B4: writing type param over a secret is refused at every scope, before any
// row is touched.
func TestNoDeclassify(t *testing.T) {
	l := params.New(servicetest.Store(t).Params)
	for _, s := range []params.Scope{org, stack, env} {
		must(t, l.Set(ctx, s, params.Entry{
			Collection: "email",
			Name:       "key",
			Kind:       params.Secret,
			Value:      "k",
		}))
		err := l.Merge(ctx, s, []params.Entry{
			{
				Collection: "email",
				Name:       "sender",
				Kind:       params.Param,
				Value:      "a@x.io",
			},
			{
				Collection: "email",
				Name:       "key",
				Kind:       params.Param,
				Value:      "open",
			},
		})
		if _, ok := errs.IsConflict(err); !ok {
			t.Errorf("%s: declassify = %v", s.Kind, err)
		}
		vals, _ := l.Values(ctx, s, true)
		if _, ok := vals["email.sender"]; ok || vals["email.key"].V != "k" {
			t.Errorf("%s: a refused merge touched rows: %v", s.Kind, vals)
		}
	}
	// Param → secret is allowed, one way.
	must(t, l.Set(ctx, env, params.Entry{
		Collection: "a",
		Name:       "b",
		Kind:       params.Param,
		Value:      "v",
	}))
	must(t, l.Set(ctx, env, params.Entry{
		Collection: "a",
		Name:       "b",
		Kind:       params.Secret,
		Value:      "v",
	}))
}

// B35: a merge next to a masked secret succeeds and keeps it.
func TestMergeKeepsSecret(t *testing.T) {
	l := params.New(servicetest.Store(t).Params)
	must(t, l.Set(ctx, stack, params.Entry{
		Collection: "db",
		Name:       "password",
		Kind:       params.Secret,
		Value:      "hunter2",
	}))
	must(t, l.Merge(ctx, stack, []params.Entry{
		{
			Collection: "db",
			Name:       "password",
			Kind:       params.Secret,
		}, // masked in the listing, sent back empty
		{
			Collection: "db",
			Name:       "user",
			Kind:       params.Param,
			Value:      "app",
		},
	}))
	vals, _ := l.Values(ctx, stack, true)
	if vals["db.password"].V != "hunter2" || vals["db.user"].V != "app" {
		t.Errorf("after merge: %v", vals)
	}
	if err := l.Set(ctx, stack, params.Entry{Collection: "Bad.Name", Name: "x", Kind: params.Param}); err == nil {
		t.Error("bad collection name accepted")
	}
}

// B37: a reader without the permission never has a secret loaded. The secret's
// ciphertext is corrupted: decrypting it would fail, so a clean list proves it
// was never read.
func TestSecretsNeverLoaded(t *testing.T) {
	st := servicetest.Store(t)
	l := params.New(st.Params)
	must(t, l.Set(ctx, env, params.Entry{
		Collection: "c",
		Name:       "s",
		Kind:       params.Secret,
		Value:      "x",
	}))
	must(t, l.Set(ctx, env, params.Entry{
		Collection: "c",
		Name:       "p",
		Kind:       params.Param,
		Value:      "y",
	}))
	if _, err := st.DB().Exec(`UPDATE params SET value = 'garbage' WHERE kind = 'secret'`); err != nil {
		t.Fatal(err)
	}
	ps, err := l.List(ctx, env, false)
	if err != nil || len(ps) != 1 || ps[0].Name != "p" {
		t.Errorf("list without secrets = %+v, %v", ps, err)
	}
	if _, err := l.List(ctx, env, true); err == nil {
		t.Error("the corrupted secret decrypted: the test proves nothing")
	}
}

func snap() params.Snapshot {
	return params.Snapshot{
		EnvParams: map[string]params.Value{
			"email.sender": {V: "env@x.io"},
			"db.pass":      {V: "s3", Secret: true},
		},
		StackParams: map[string]params.Value{
			"email.sender": {V: "stack@x.io"},
			"email.host":   {V: "smtp"},
			"domains.base": {V: "x.io"},
		},
		OrgParams: map[string]params.Value{"email.org_only": {V: "org"}},
		Env:       "dev",
		Stackr:    map[string]string{params.ProxyIP: "10.0.0.1"},
		Backups:   map[string]string{"s3-main": "s3://b"},
		Self:      params.Source{Outputs: map[string]string{"host": "api"}},
		Tiles: map[string]params.Source{
			"api-db": {
				Slice:    true,
				Attached: true,
				Network:  "net-pg",
				Outputs:  map[string]string{"DATABASE_URL": "pg://api-db"},
			},
			"mq": {
				Slice:   true,
				Outputs: map[string]string{"url": "amqp://mq"},
			},
			"api": {
				Outputs: params.Endpoint{Alias: "api", Port: 80}.Outputs(),
			},
		},
	}
}

// B5 plus the grammar: one row per ref shape, where it sits, and the answer.
func TestResolve(t *testing.T) {
	for _, c := range []struct {
		where params.Where
		in    string
		want  string // "" with err set
		err   string // substring; "unset" means errs.Unset
	}{
		{params.InEnv, "plain", "plain", ""},
		{params.InEnv, "${{ params.email.sender }}", "env@x.io", ""},  // B5: env before stack
		{params.InEnv, "${{params.email.host}}", "smtp", ""},          // falls to stack
		{params.InEnv, "${{ params.email.org_only }}", "", "unset"},   // B5: never reaches org
		{params.InEnv, "${{ org.params.email.org_only }}", "org", ""}, // deliberately
		{params.InEnv, "${{ params.email.nope }}", "", "unset"},
		{params.InEnv, "${{ self.host }}", "api", ""},
		{params.InEnv, "${{ tile.api-db.DATABASE_URL }}", "pg://api-db", ""},
		{params.InEnv, "${{ tile.mq.url }}", "", "slice mq is not bound to this tile"},
		{params.InEnv, "${{ tile.api-db.database-url }}", "", "not a valid name"},
		{params.InEnv, "${{ tile.api.url }}", "http://api:80", ""},
		{params.InEnv, "${{ tile.api.public_url }}", "", "no output"},
		{params.InEnv, "${{ tile.ghost.url }}", "", "no tile"},
		{params.InEnv, "${{ stack.cache.host }}", "", "stack.cache refs are gone; declare a slice tile, see DECIDE 194"},
		{params.InEnv, "${{ org.cache.host }}", "", "org.cache refs are gone; declare a slice tile, see DECIDE 194"},
		{params.InEnv, "${{ stackr.PROXY_IP }}", "10.0.0.1", ""},
		{params.InEnv, "${{ stackr.PROXY_CIDR }}", "", "not known yet"},
		{params.InEnv, "${{ stackr.HOME }}", "", "not a stackr value"},
		{params.InEnv, "${{ tile.params.url }}", "", "reserved"},
		{params.InEnv, "${{ env.x }}", "", "the only env ref"},
		{params.InEnv, "${{ env.name }}", "", "not allowed in env"},
		{params.InProvisionFrom, "infra:${{ env.name }}:pg-db", "infra:dev:pg-db", ""},
		{params.InProvisionFrom, "${{ params.email.host }}:dev:pg-db", "smtp:dev:pg-db", ""},
		{params.InProvisionFrom, "${{ tile.api.host }}:dev:pg-db", "", "not allowed in provision_from"},
		{params.InEnv, "${{ params.Email.x }}", "", "not a valid collection"},
		{params.InEnv, "${{ org.backups.s3-main }}", "", "not allowed in env"},
		{params.InBackupDest, "${{ org.backups.s3-main }}", "s3://b", ""},
		{params.InBackupDest, "${{ params.email.host }}", "", "not allowed"},
		{params.InDomain, "api.${{ params.domains.base }}", "api.x.io", ""},
		{params.InDomain, "${{ params.db.pass }}", "", "is a secret"},
		{params.InDomain, "${{ tile.api.host }}", "", "not allowed in domain"},
		{params.InCommand, "run --db ${{ tile.api-db.DATABASE_URL }} ${{ params.email.sender }}", "run --db pg://api-db env@x.io", ""},
		{params.InEnv, "${{ params.email.sender }}-${{ params.email.nope }}", "", "unset"}, // never half-expanded
	} {
		got, err := params.NewResolver(snap()).Expand(c.where, c.in)
		var unset errs.Unset
		switch {
		case c.err == "unset" && !errors.As(err, &unset):
			t.Errorf("%s %q = %q, %v; want unset", c.where, c.in, got, err)
		case c.err != "" && c.err != "unset" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s %q = %q, %v; want error %q", c.where, c.in, got, err, c.err)
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%s %q = %q, %v; want %q", c.where, c.in, got, err, c.want)
		case c.err != "" && got != "":
			t.Errorf("%s %q: an error still returned %q", c.where, c.in, got)
		}
	}
	rr := params.NewResolver(snap())
	_, _ = rr.Expand(params.InEnv, "${{ tile.api.host }} ${{ tile.api-db.DATABASE_URL }}")
	if d, n := rr.Deps(), rr.Networks(); strings.Join(d, ",") != "api,api-db" || strings.Join(n, ",") != "net-pg" {
		t.Errorf("deps %v, networks %v", d, n)
	}
	var gone params.Removed
	if _, err := params.Parse("stack.cache.host"); !errors.As(err, &gone) || gone.Form != "stack.cache" {
		t.Errorf("stack ref = %v, want Removed", err)
	}
}

func TestGenerate(t *testing.T) {
	a, b := params.Generate(32), params.Generate(32)
	if len(a) != 32 || a == b ||
		strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		t.Errorf("generate = %q, %q", a, b)
	}
}
