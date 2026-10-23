package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 033 on a database as the rig has it (032): GitHub connectors,
// org and server, one shared, and a Namecheap one managing a domain with a
// record, every settings blob really sealed. After it and the start, every
// row and link is kept, the Namecheap account opens with its key, and the
// GitHub App's webhook secret still verifies.
func TestConnectorsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stackr.db")
	db, err := sqlite.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := appdb.MigrateConfig()
	if err := sqlite.Migrate(db, cfg); err != nil {
		t.Fatal(err)
	}
	v, _, err := sqlite.MigrateVersion(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.MigrateSteps(db, cfg, 32-int(v)); err != nil {
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
	const ncKey = "nc-key-migrated-0033"
	now := time.Now()
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO orgs (id,name,slug,avatar_path,env_colors,settings,setup_mode,created_at,
			config_connector_id,config_repo,config_branch,config_path)
			VALUES ('o1','Acme','acme','','','{}','',?,'','','','')`, []any{now}},
		{`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at,share_all)
			VALUES ('g1',NULL,'github','gh-srv','github.com',?,?,0)`,
			[]any{seal(`{"app":{"id":2,"slug":"stackr-srv","webhook_secret":"srv-hook"}}`), now}},
		{`INSERT INTO connector_shares VALUES ('g1','o1')`, nil},
		{`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at)
			VALUES ('g2','o1','github','gh','github.com',?,?)`,
			[]any{seal(`{"app":{"id":1,"slug":"stackr-acme","webhook_secret":"org-hook"}}`), now}},
		{`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at,capabilities)
			VALUES ('n1','o1','namecheap','Namecheap · jo','api.namecheap.com',?,?,'["dns"]')`,
			[]any{seal(`{"dns":{"user":"jo","key":"` + ncKey + `","client_ip":"203.0.113.7"}}`), now}},
		{`INSERT INTO dns_domains (id,connector_id,org_id,name,share_all,org_ids,created_at)
			VALUES ('d1','n1','o1','acme.dev',0,'[]',?)`, []any{now}},
		{`INSERT INTO dns_records (id,dns_domain_id,zone,name,type,value,ttl,created_at)
			VALUES ('r1','d1','acme.dev','@','A','198.51.100.1',300,?)`, []any{now}},
	} {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
	// 033 itself, before a start's records sync tidies the record away
	if err := sqlite.MigrateSteps(db, cfg, 1); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"connectors": 3, "connector_shares": 1, "dns_domains": 1, "dns_records": 1} {
		var n int
		if err := db.Get(&n, "SELECT count(*) FROM "+table); err != nil || n != want {
			t.Errorf("%s = %d %v, want %d", table, n, err, want)
		}
	}
	var link string
	if err := db.Get(&link, "SELECT dns_domain_id FROM dns_records WHERE id = 'r1'"); err != nil || link != "d1" {
		t.Errorf("record's domain = %q %v", link, err)
	}
	var dangling int
	if err := db.Get(&dangling, "SELECT count(*) FROM pragma_foreign_key_check"); err != nil || dangling != 0 {
		t.Errorf("foreign_key_check = %d %v", dangling, err)
	}
	var kinds []string
	if err := db.Select(&kinds, "SELECT kind || ' ' || host FROM connectors ORDER BY id"); err != nil ||
		strings.Join(kinds, ",") != "github github.com,github github.com,namecheap namecheap" {
		t.Errorf("kind and host = %v %v", kinds, err)
	}
	_ = db.Close()

	env := servicetest.NewWith(t, nil, func(c *service.Config) { c.DBPath = path })
	env.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}

	// Namecheap: the same row, its settings out of the old envelope, the
	// account opened with its key
	o1 := "o1"
	cs, err := env.Orch.DNSConnectors(ctx, &o1)
	if err != nil || len(cs) != 1 || cs[0].ID != "n1" || cs[0].Kind != "namecheap" {
		t.Fatalf("dns connectors = %+v %v", cs, err)
	}
	ds, err := env.Orch.DNSDomains(ctx, &o1, "n1")
	if err != nil || len(ds) != 1 || ds[0].State != service.DNSManaged {
		t.Fatalf("domains = %+v %v", ds, err)
	}
	if o := env.DNS.Opened[len(env.DNS.Opened)-1]; o["key"] != ncKey || o["user"] != "jo" || o["client_ip"] != "203.0.113.7" {
		t.Errorf("opened with user %q ip %q, key kept %v", o["user"], o["client_ip"], o["key"] == ncKey)
	}
	n1, err := env.Store.Connectors.Get(ctx, "n1")
	if err != nil || strings.Contains(n1.Settings, `"dns"`) || !strings.Contains(n1.Settings, `"client_ip"`) {
		t.Errorf("namecheap settings not reshaped (%v)", err)
	}

	// GitHub: both Apps still verify their webhooks; the server one is
	// still shared with the org
	for id, secret := range map[string]string{"g1": "srv-hook", "g2": "org-hook"} {
		body := []byte(`{"zen":"hi"}`)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if err := env.Orch.Webhook(ctx, id, "ping", "sha256="+hex.EncodeToString(mac.Sum(nil)), body); err != nil {
			t.Errorf("%s webhook: %v", id, err)
		}
		if err := env.Orch.Webhook(ctx, id, "ping", "sha256=00", body); err == nil {
			t.Errorf("%s webhook verified a wrong signature", id)
		}
	}
	srv, err := env.Orch.ServerConnectors(ctx)
	if err != nil || len(srv) != 1 || srv[0].ID != "g1" || len(srv[0].OrgIDs) != 1 || srv[0].OrgIDs[0] != "o1" {
		t.Errorf("server connectors = %+v %v", srv, err)
	}
	if u, err := env.Orch.ConnectorInstallURL(ctx, "o1", "g2"); err != nil || !strings.Contains(u, "stackr-acme") {
		t.Errorf("org App = %q %v", u, err)
	}
}
