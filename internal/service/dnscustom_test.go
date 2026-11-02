package service_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// customRig is recordsRig with shop.acme.dev on the tile; it returns the
// server connector's id and the tile domain.
func customRig(t *testing.T) (*servicetest.Env, string, servicetest.Tile, service.Domain) {
	t.Helper()
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	cs, err := env.Orch.DNSConnectors(ctx, nil)
	if err != nil || len(cs) != 1 {
		t.Fatalf("connectors %v %v", cs, err)
	}
	d, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"})
	if err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	return env, cs[0].ID, tl, d
}

func count(rs []libdns.RR, name, typ, data string) int {
	n := 0
	for _, r := range rs {
		if r.Name == name && r.Type == typ && strings.TrimSuffix(r.Data, ".") == data {
			n++
		}
	}
	return n
}

func conflict(t *testing.T, err error, has string) {
	t.Helper()
	c, ok := errs.IsConflict(err)
	if !ok || !strings.Contains(c.Msg, has) {
		t.Fatalf("want a conflict with %q, got %v", has, err)
	}
}

// Decision 6: TXT and MX go next to what a name has, the same record by
// hand is taken over (no duplicate, and it then goes with the hostname),
// and a CNAME wants its name to itself, both ways.
func TestCustomRecordsAlongside(t *testing.T) {
	env, _, tl, d := customRig(t)
	ctx := context.Background()
	env.DNS.Put("acme.dev",
		libdns.RR{Name: "shop", Type: "TXT", Data: "hand"},
		libdns.RR{Name: "shop", Type: "TXT", Data: "v=same"},
		libdns.RR{Name: "blog.shop", Type: "TXT", Data: "x"},
	)
	add := func(name, typ, value string) (service.CustomRecord, error) {
		return env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Name: name, Type: typ, Value: value})
	}
	r, err := add("", "TXT", "google-site-verification=1")
	if err != nil || r.Note != "added (3 others there stay)" || !r.Written {
		t.Fatalf("alongside = %+v %v", r, err)
	}
	same, err := add("@", "TXT", "v=same")
	if err != nil || same.Note != "taken over" {
		t.Fatalf("take over = %+v %v", same, err)
	}
	if _, err := add("", "MX", "10 mx.example.net"); err != nil {
		t.Fatal(err)
	}
	z := env.DNS.Zone("acme.dev")
	if count(z, "shop", "TXT", "v=same") != 1 || count(z, "shop", "TXT", "google-site-verification=1") != 1 ||
		count(z, "shop", "TXT", "hand") != 1 || count(z, "shop", "MX", "10 mx.example.net") != 1 {
		t.Fatalf("zone after adds: %v", z)
	}
	if _, err := add("", "TXT", "v=same"); err == nil {
		t.Fatal("the same record twice was taken")
	}

	_, err = add("blog", "CNAME", "blogs.example.net")
	conflict(t, err, "a CNAME needs the name to itself")
	if _, err := add("cdn", "CNAME", "Edge.Example.NET."); err != nil {
		t.Fatal(err)
	}
	_, err = add("cdn", "TXT", "x")
	conflict(t, err, "a name with a CNAME takes no other record")
	if count(env.DNS.Zone("acme.dev"), "cdn.shop", "CNAME", "edge.example.net") != 1 {
		t.Fatalf("CNAME not written: %v", env.DNS.Zone("acme.dev"))
	}

	// the taken-over record is stackr's now: removing it deletes it; the
	// hand one next to it stays
	if err := env.Orch.RemoveTileDomainRecord(ctx, tl.ID, d.ID, same.ID); err != nil {
		t.Fatal(err)
	}
	if z := env.DNS.Zone("acme.dev"); count(z, "shop", "TXT", "v=same") != 0 || count(z, "shop", "TXT", "hand") != 1 {
		t.Fatalf("after remove: %v", z)
	}

	// they live and die with the hostname
	if err := env.Orch.DetachDomain(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	z = env.DNS.Zone("acme.dev")
	if count(z, "shop", "TXT", "google-site-verification=1") != 0 || count(z, "cdn.shop", "CNAME", "edge.example.net") != 0 ||
		count(z, "shop", "TXT", "hand") != 1 || count(z, "blog.shop", "TXT", "x") != 1 {
		t.Fatalf("after detach: %v", z)
	}
	if rs, _ := env.Store.DNSRecords.List(ctx); len(rs) != 0 {
		t.Errorf("rows after detach: %v", rs)
	}
}

// Decision 7: a name is an app hostname or a custom record (TXT and MX
// aside), refused both ways with the same message. Decision 1: an app's
// records stay on its own name and below, never another app's.
func TestCustomRecordNameClash(t *testing.T) {
	env, conn, tl, d := customRig(t)
	ctx := context.Background()
	_, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "A", Value: "203.0.113.5"})
	conflict(t, err, "shop.acme.dev is an app hostname")
	if _, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "TXT", Value: "ok"}); err != nil {
		t.Fatal(err)
	}

	// a domain CNAME on api, then an app on api: refused
	if _, err := env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", service.CustomRecordIn{Name: "api", Type: "CNAME", Value: "api.example.net"}); err != nil {
		t.Fatal(err)
	}
	_, err = env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "api.acme.dev"})
	one, _ := errs.IsConflict(err)
	// an app on web, then a domain CNAME on web: the same refusal
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "web.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	_, err = env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", service.CustomRecordIn{Name: "web", Type: "CNAME", Value: "web.example.net"})
	other, _ := errs.IsConflict(err)
	if one.Msg == "" || strings.ReplaceAll(one.Msg, "api.", "web.") != other.Msg {
		t.Fatalf("not the same message both ways:\n%q\n%q", one.Msg, other.Msg)
	}
	if _, err := env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", service.CustomRecordIn{Name: "web", Type: "TXT", Value: "v"}); err != nil {
		t.Fatalf("a domain TXT next to an app: %v", err)
	}

	// another app's hostname under shop's
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "api.shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	ds, _ := env.Orch.Domains(ctx, tl.ID)
	i := slices.IndexFunc(ds, func(x service.Domain) bool { return x.Host == "shop.acme.dev" })
	_, err = env.Orch.AddTileDomainRecord(ctx, tl.ID, ds[i].ID, service.CustomRecordIn{Name: "api", Type: "TXT", Value: "v"})
	conflict(t, err, "another app's hostname")
}

// Decisions 2, 4 and 8: a record on a name stackr does not manage, outside
// its scope, or on a domain the org may not use is refused naming both.
func TestCustomRecordNotManaged(t *testing.T) {
	env, conn, tl, d := customRig(t)
	ctx := context.Background()
	other, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.Orch.AddTileDomainRecord(ctx, tl.ID, other.ID, service.CustomRecordIn{Type: "TXT", Value: "v"})
	conflict(t, err, "shop.example.org is not in a managed domain: stackr does not manage example.org")

	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, conn, "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	// the narrowing marks shop's A handed back: kept for the Domains page,
	// never in the managed list
	if back, _ := env.Store.DNSRecords.ListHandedBack(ctx); len(back) != 1 || back[0].Name != "shop" || back[0].HandedBackAt == nil {
		t.Errorf("handed back = %+v", back)
	}
	_, err = env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "TXT", Value: "v"})
	conflict(t, err, "shop.acme.dev is outside the part of acme.dev stackr manages (only under test.acme.dev)")
	_, err = env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", service.CustomRecordIn{Type: "MX", Value: "10 mx.example.net"})
	conflict(t, err, "acme.dev is outside the part of acme.dev stackr manages")

	// shop.io for the other org only
	orgs, _ := env.Store.Orgs.List(ctx)
	ids := map[string]string{}
	for _, o := range orgs {
		ids[o.Slug] = o.ID
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, conn, "shop.io", []string{ids["other"]}); err != nil {
		t.Fatal(err)
	}
	acme := ids["acme"]
	_, _, err = env.Orch.CheckFileRecords(ctx, acme, "x.shop.io", nil, nil, true, nil)
	conflict(t, err, "shop.io is managed but not available to org acme")
	_, _, err = env.Orch.CheckFileRecords(ctx, acme, "x.example.org", nil, nil, true, nil)
	conflict(t, err, "x.example.org is not in a managed domain")
	if _, _, err := env.Orch.CheckFileRecords(ctx, acme, "x.example.org", nil, nil, false, nil); err != nil {
		t.Errorf("no dns: managed, no records: %v", err)
	}
	_, _, err = env.Orch.CheckFileRecords(ctx, acme, "x.example.org", []store.CustomRecord{{Type: "TXT", Value: "v"}}, nil, false, nil)
	conflict(t, err, "x.example.org is not in a managed domain")

	// a file's records on a host in the managed part: checked and noted;
	// one on a hostname the same file adds for another app is refused
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, conn, "acme.dev", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	env.DNS.Put("acme.dev", libdns.RR{Name: "new", Type: "TXT", Data: "hand"})
	recs, notes, err := env.Orch.CheckFileRecords(ctx, acme, "new.acme.dev", []store.CustomRecord{{Type: "txt", Value: "v=1"}}, nil, false, nil)
	if err != nil || len(recs) != 1 || recs[0].ID == "" || recs[0].Type != "TXT" || len(notes) != 2 ||
		notes[0] != "Managed by acme.dev via Namecheap (server)" || !strings.Contains(notes[1], "added (1 other there stays)") {
		t.Fatalf("file records = %+v %v %v", recs, notes, err)
	}
	_, _, err = env.Orch.CheckFileRecords(ctx, acme, "new.acme.dev", []store.CustomRecord{{Name: "api", Type: "TXT", Value: "v"}}, nil, false, []string{"api.new.acme.dev"})
	conflict(t, err, "another app's hostname")
	_, _, err = env.Orch.CheckFileRecords(ctx, acme, "new.acme.dev", []store.CustomRecord{{Type: "A", Value: "1.2.3"}}, nil, false, nil)
	if _, ok := errs.IsInvalid(err); !ok {
		t.Errorf("a bad A value: %v", err)
	}
}

// Decision 5: a declared record changed or deleted at the DNS host is put
// back on the next records sync, and the drawer says so. An edited CNAME
// is replaced, never doubled.
func TestCustomRecordPutBack(t *testing.T) {
	env, conn, tl, d := customRig(t)
	ctx := context.Background()
	if _, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Name: "_dmarc", Type: "TXT", Value: "v=DMARC1; p=none"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", service.CustomRecordIn{Name: "docs", Type: "CNAME", Value: "docs.example.net"}); err != nil {
		t.Fatal(err)
	}
	env.DNS.Records["acme.dev"] = slices.DeleteFunc(env.DNS.Records["acme.dev"], func(r libdns.RR) bool { return r.Name == "_dmarc.shop" })
	for i, r := range env.DNS.Records["acme.dev"] {
		if r.Name == "docs" {
			env.DNS.Records["acme.dev"][i].Data = "elsewhere.example.net"
		}
	}
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	z := env.DNS.Zone("acme.dev")
	if count(z, "_dmarc.shop", "TXT", "v=DMARC1; p=none") != 1 || count(z, "docs", "CNAME", "docs.example.net") != 1 ||
		count(z, "docs", "CNAME", "elsewhere.example.net") != 0 {
		t.Fatalf("not put back: %v", z)
	}
	rs, err := env.Orch.TileDomainRecords(ctx, tl.ID, d.ID)
	if err != nil || len(rs.Records) != 1 || rs.Records[0].PutBackAt == nil || !rs.Records[0].Written {
		t.Fatalf("drawer = %+v %v", rs, err)
	}
	if rs.Source != "Managed by acme.dev via Namecheap (server)" {
		t.Errorf("source = %q", rs.Source)
	}
	dr, err := env.Orch.DNSDomainRecords(ctx, nil, conn, "acme.dev")
	if err != nil || len(dr.Records) != 1 || dr.Records[0].PutBackAt == nil {
		t.Fatalf("domain records = %+v %v", dr, err)
	}
	// app A records keep today's rule: a hand edit is the user's
	rows, _ := env.Store.DNSRecords.List(ctx)
	for _, r := range rows {
		want := store.RecordCustom
		if r.Type == "A" {
			want = store.RecordApp
		}
		if r.Kind != want {
			t.Errorf("%s %s kind %q", r.Type, r.Name, r.Kind)
		}
	}
}

// A zone whose mail setting drops MX records (Namecheap mail forwarding)
// refuses a custom MX on every path before anything is saved; a TXT still
// goes. An MX stackr already has there is reported by the sync, never put
// back run after run.
func TestCustomMXRefusedWithoutCustomMX(t *testing.T) {
	env, conn, tl, d := customRig(t)
	ctx := context.Background()
	const why = "This domain uses Namecheap mail forwarding. Switch Mail Settings to Custom MX first."
	mx := service.CustomRecordIn{Type: "MX", Value: "10 mx.example.net"}

	// one MX written while the zone took them, then the mail setting flips
	if _, err := env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", service.CustomRecordIn{Type: "MX", Value: "20 old.example.net"}); err != nil {
		t.Fatal(err)
	}
	env.DNS.NoMX("acme.dev", why)

	_, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, mx)
	conflict(t, err, why)
	_, err = env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", mx)
	conflict(t, err, why)
	orgs, _ := env.Store.Orgs.List(ctx)
	acme := ""
	for _, o := range orgs {
		if o.Slug == "acme" {
			acme = o.ID
		}
	}
	_, _, err = env.Orch.CheckFileRecords(ctx, acme, "new.acme.dev", []store.CustomRecord{{Type: "MX", Value: "10 mx.example.net"}}, nil, false, nil)
	conflict(t, err, why)
	if rs, _ := env.Orch.TileDomainRecords(ctx, tl.ID, d.ID); len(rs.Records) != 0 {
		t.Errorf("refused MX saved: %+v", rs.Records)
	}
	if dr, _ := env.Orch.DNSDomainRecords(ctx, nil, conn, "acme.dev"); len(dr.Records) != 1 {
		t.Errorf("refused MX saved on the domain: %+v", dr.Records)
	}

	// the sync: the MX it has reads gone (Namecheap hides it); it says why
	// and writes nothing, run after run
	before, _, _ := env.DNS.Log()
	for range 2 {
		if err := env.Orch.SyncProxy(ctx); err != nil {
			t.Fatal(err)
		}
		env.Orch.WaitDNS()
		if got := env.Orch.DNSSyncError(); !strings.Contains(got, why) || !strings.Contains(got, "MX on acme.dev not written") {
			t.Fatalf("sync error = %q", got)
		}
	}
	after, _, _ := env.DNS.Log()
	for _, a := range after[len(before):] {
		if strings.Contains(a, "old.example.net") {
			t.Errorf("MX put back: %s", a)
		}
	}
	rows, _ := env.Store.DNSRecords.List(ctx)
	for _, x := range rows {
		if x.Type == "MX" && x.PutBackAt != nil {
			t.Errorf("MX row marked put back: %+v", x)
		}
	}
	// read back, the MX says it is not live and why, from the last sync:
	// no provider call
	calls := env.DNS.Calls
	dr, err := env.Orch.DNSDomainRecords(ctx, nil, conn, "acme.dev")
	if err != nil || len(dr.Records) != 1 || dr.Records[0].Written || dr.Records[0].NotLive != why {
		t.Errorf("MX read back = %+v %v", dr.Records, err)
	}
	md, err := env.Orch.ManagedDomainNamed(ctx, nil, "acme.dev")
	if i := slices.IndexFunc(md.Records, func(r service.ManagedRecord) bool { return r.Type == "MX" }); err != nil || i < 0 ||
		md.Records[i].Drift != "Not live: this domain uses Namecheap mail forwarding. Switch Mail Settings to Custom MX first." {
		t.Errorf("Domains page MX = %+v %v", md.Records, err)
	}
	if env.DNS.Calls != calls {
		t.Errorf("reading records called the provider %d times", env.DNS.Calls-calls)
	}

	// a TXT in the same zone is written; the MX's reason is not its own
	r, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "TXT", Value: "v=1"})
	if r.ID == "" || err != nil || !r.Written || strings.Contains(r.Note, why) {
		t.Fatalf("TXT add = %+v %v", r, err)
	}
	if count(env.DNS.Zone("acme.dev"), "shop", "TXT", "v=1") != 1 {
		t.Errorf("TXT not written: %v", env.DNS.Zone("acme.dev"))
	}

	// back on Custom MX: the next sync clears the reason, the row is live
	env.DNS.NoMX("acme.dev", "")
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if dr, _ := env.Orch.DNSDomainRecords(ctx, nil, conn, "acme.dev"); len(dr.Records) != 1 || !dr.Records[0].Written || dr.Records[0].NotLive != "" {
		t.Errorf("MX after Custom MX = %+v", dr.Records)
	}
}

// Moving a hostname into a zone whose mail setting drops MX records is
// refused when it carries an MX, as adding one there is; without an MX it
// moves.
func TestMoveRefusesMXWithoutCustomMX(t *testing.T) {
	env, conn, tl, d := customRig(t)
	ctx := context.Background()
	const why = "This domain uses Namecheap mail forwarding. Switch Mail Settings to Custom MX first."
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, conn, "shop.io", nil); err != nil {
		t.Fatal(err)
	}
	env.DNS.NoMX("shop.io", why)
	if _, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "TXT", Value: "v=1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.UpdateDomain(ctx, d.ID, service.DomainSpec{Host: "a.shop.io"}); err != nil {
		t.Fatalf("TXT only move: %v", err)
	}
	if _, err := env.Orch.UpdateDomain(ctx, d.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "MX", Value: "10 mx.example.net"}); err != nil {
		t.Fatal(err)
	}
	_, err := env.Orch.UpdateDomain(ctx, d.ID, service.DomainSpec{Host: "b.shop.io"})
	conflict(t, err, why)
	if got, _ := env.Store.Domains.Get(ctx, d.ID); got.Host != "shop.acme.dev" {
		t.Errorf("refused move saved: %s", got.Host)
	}
	// a stack file renaming the host: a new entry, checked as an add
	orgs, _ := env.Store.Orgs.List(ctx)
	i := slices.IndexFunc(orgs, func(o store.Org) bool { return o.Slug == "acme" })
	_, _, err = env.Orch.CheckFileRecords(ctx, orgs[i].ID, "b.shop.io", []store.CustomRecord{{Type: "MX", Value: "10 mx.example.net"}}, nil, false, nil)
	conflict(t, err, why)
}

// A rename moving a generated hostname with an MX is never refused for the
// zone's mail setting: the rename is committed, the MX reads not live.
func TestRenameMovesMXWithoutProviderRead(t *testing.T) {
	env, _, tl, _ := customRig(t)
	ctx := context.Background()
	d, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Auto: true})
	if err != nil || !strings.HasSuffix(d.Host, ".acme.dev") {
		t.Fatalf("auto domain = %+v %v", d, err)
	}
	if _, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "MX", Value: "10 mx.example.net"}); err != nil {
		t.Fatal(err)
	}
	env.DNS.NoMX("acme.dev", "forwarding")
	calls := env.DNS.Calls
	if _, _, err := env.Orch.RenameTile(ctx, tl.ID, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got, _ := env.Store.Domains.Get(ctx, d.ID); got.Host == d.Host {
		t.Errorf("host did not move: %s", got.Host)
	}
	if env.DNS.Calls != calls {
		t.Errorf("the rename read the provider %d times", env.DNS.Calls-calls)
	}
}

// An add the sync cannot write is saved, not an error: it answers written
// false with the reason, and the next sync writes it.
func TestAddRecordSyncFailureIsANote(t *testing.T) {
	env, conn, tl, d := customRig(t)
	ctx := context.Background()
	env.DNS.Fail(errors.New("provider down"))
	r, err := env.Orch.AddTileDomainRecord(ctx, tl.ID, d.ID, service.CustomRecordIn{Type: "TXT", Value: "v=1"})
	if err != nil || r.ID == "" || r.Written || !strings.Contains(r.Note, "provider down") {
		t.Fatalf("tile add = %+v %v", r, err)
	}
	r, err = env.Orch.AddDNSDomainRecord(ctx, nil, conn, "acme.dev", service.CustomRecordIn{Name: "verify", Type: "TXT", Value: "v=1"})
	if err != nil || r.ID == "" || r.Written || !strings.Contains(r.Note, "provider down") {
		t.Fatalf("domain add = %+v %v", r, err)
	}
	env.DNS.Fail(nil)
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if rs, _ := env.Orch.TileDomainRecords(ctx, tl.ID, d.ID); len(rs.Records) != 1 || !rs.Records[0].Written {
		t.Errorf("after the provider is back: %+v", rs.Records)
	}
}
