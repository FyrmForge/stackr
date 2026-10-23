package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// GitHub rows as the rig has them on 033: the leaf's old envelope sealed
// in settings ({"app": {...}} with a real private key, {"state", "user"}
// for a handshake begun), a server App shared with all orgs, another
// shared with one by name, and an org App. After a start each keeps its
// id, name, host, sharing and key; its webhook still verifies, its
// install page is its slug's; the begun one stays pending, finishable
// only by its user, and swept when old.
func TestGitHubRowsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stackr.db")
	db, err := sqlite.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(db, appdb.MigrateConfig()); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(servicetest.Key)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(s string) string {
		v, err := box.Encrypt(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := strings.ReplaceAll(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), "\n", `\n`)
	app := func(id, slug, secret string) string {
		return seal(`{"app":{"id":` + id + `,"slug":"` + slug + `","pem":"` + pemKey + `","webhook_secret":"` + secret + `"}}`)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO orgs (id,name,slug,avatar_path,env_colors,settings,setup_mode,created_at,
			config_connector_id,config_repo,config_branch,config_path)
			VALUES ('o1','Acme','acme','','','{}','',?,'','','',''), ('o2','Globex','globex','','','{}','',?,'','','','')`, []any{old, old}},
		{`INSERT INTO connectors (id,org_id,kind,name,host,settings,created_at,share_all,capabilities)
			VALUES ('srv',NULL,'github','GitHub · stackr-rig','github.com',?,?,1,'["git"]')`, []any{app("11", "stackr-rig", "srv-hook"), old}},
		{`INSERT INTO connectors (id,org_id,kind,name,host,settings,created_at,share_all,capabilities)
			VALUES ('one',NULL,'github','gh-one','github.com',?,?,0,'["git"]')`, []any{app("12", "stackr-one", "one-hook"), old}},
		{`INSERT INTO connector_shares VALUES ('one','o2')`, nil},
		{`INSERT INTO connectors (id,org_id,kind,name,host,settings,created_at,share_all,capabilities)
			VALUES ('org','o1','github','GitHub · stackr-acme','github.com',?,?,0,'["git"]')`, []any{app("13", "stackr-acme", "org-hook"), old}},
		{`INSERT INTO connectors (id,org_id,kind,name,host,settings,created_at,share_all,capabilities)
			VALUES ('half',NULL,'github','GitHub (connecting… half0000)','github.com',?,?,1,'["git"]')`,
			[]any{seal(`{"state":"n0nce","user":"u1"}`), time.Now()}},
	} {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	_ = db.Close()

	env := servicetest.NewWith(t, nil, func(c *service.Config) { c.DBPath = path })
	srv, err := env.Orch.ServerConnectors(ctx)
	if err != nil || len(srv) != 3 {
		t.Fatalf("server connectors = %+v %v", srv, err)
	}
	byID := map[string]service.ServerConnector{}
	for _, c := range srv {
		byID[c.ID] = c
	}
	if c := byID["srv"]; !c.ShareAll || c.Name != "GitHub · stackr-rig" || c.Host != "github.com" || c.Pending {
		t.Errorf("rig App = %+v", c)
	}
	if c := byID["one"]; c.ShareAll || len(c.OrgIDs) != 1 || c.OrgIDs[0] != "o2" {
		t.Errorf("named share = %+v", c)
	}
	if c := byID["half"]; !c.Pending {
		t.Errorf("begun handshake = %+v, want pending", c)
	}
	for id, secret := range map[string]string{"srv": "srv-hook", "one": "one-hook", "org": "org-hook"} {
		body := []byte(`{"zen":"hi"}`)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if err := env.Orch.Webhook(ctx, id, hook("ping", "sha256="+hex.EncodeToString(mac.Sum(nil))), body); err != nil {
			t.Errorf("%s webhook: %v", id, err)
		}
	}
	if err := env.Orch.Webhook(ctx, "half", hook("ping", "sha256=00"), nil); !errors.Is(err, service.ErrBadSignature) {
		t.Errorf("pending webhook = %v", err)
	}
	if u, _ := env.Orch.ServerConnectorInstallURL(ctx, "srv"); u != "https://github.com/apps/stackr-rig/installations/new" {
		t.Errorf("install = %q", u)
	}
	if u, _ := env.Orch.ConnectorInstallURL(ctx, "o1", "org"); !strings.Contains(u, "stackr-acme") {
		t.Errorf("org install = %q", u)
	}
	for _, id := range []string{"srv", "one", "org"} {
		c, err := env.Store.Connectors.Get(ctx, id)
		if err != nil || !strings.Contains(c.Settings, "BEGIN RSA PRIVATE KEY") || strings.Contains(c.Settings, `"app"`) {
			t.Errorf("%s: key lost or blob not reshaped (%v)", id, err)
		}
	}
	// shared with all: both orgs read it; named: only Globex
	if cs, _ := env.Orch.ConnectedConnectors(ctx, "o1"); len(cs) != 2 {
		t.Errorf("acme reads %d, want its own and the rig App", len(cs))
	}
	if cs, _ := env.Orch.ConnectedConnectors(ctx, "o2"); len(cs) != 2 {
		t.Errorf("globex reads %d, want the rig App and gh-one", len(cs))
	}
	// the begun handshake is still its user's to finish
	if _, err := env.Orch.ConnectorBack(ctx, "u2", url.Values{"state": {"half.n0nce"}, "code": {"c"}}); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("another user's callback = %v", err)
	}
	if n, err := env.Orch.SweepPendingConnectors(ctx, time.Now().Add(2*time.Hour)); err != nil || n != 1 {
		t.Errorf("sweep = %d %v, want the begun one", n, err)
	}
}

// An App added through the core (its existing-App form) is a git
// connection on its provider's host, so a repo URL there resolves to it;
// a server one is shared with every org; the key never comes back.
func TestGitConnectionThroughTheCore(t *testing.T) {
	env, ctx := servicetest.New(t), context.Background()
	org := env.Org(t, "acme")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	a, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "github", Alt: true, Values: map[string]string{
		"app_id": "42", "slug": "my-app", "private_key": pemKey, "webhook_secret": "w",
	}})
	if err != nil || a.Out != nil || a.Connector.Host != "github.com" || !a.Connector.ShareAll || a.Connector.Settings != "" {
		t.Fatalf("add = %+v %v", a, err)
	}
	cs, err := env.Orch.ConnectedConnectors(ctx, org)
	if err != nil || len(cs) != 1 || cs[0].ID != a.Connector.ID {
		t.Fatalf("the org reads %+v %v", cs, err)
	}
	// owner/name is spelled by the connection that owns it
	if st, err := env.Orch.SetConfigRepo(ctx, env.Stack(t, org, "shop").Stack, a.Connector.ID, "acme/shop", "main", ""); err != nil || st.ConfigRepo != "https://github.com/acme/shop" {
		t.Fatalf("bind = %q %v", st.ConfigRepo, err)
	}
	// the manifest flow instead waits pending, shared from the start
	b, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "github", Values: map[string]string{"owner": "acme"}, User: "u1", Return: "/-/admin"})
	if err != nil || b.Out == nil || !b.Connector.Pending || !b.Connector.ShareAll {
		t.Fatalf("manifest add = %+v %v", b, err)
	}
}
