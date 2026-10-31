package promote

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A domain entry's records: and dns: managed go through CheckDNS: its
// refusal is a blocker naming the tile and host, its notes ride on the
// change row, the records land on the row, a re-plan is clean, and the
// export writes them back.
func TestFileRecords(t *testing.T) {
	w := setup(t)
	var seen struct {
		managed bool
		recs    []store.CustomRecord
	}
	w.f.CheckDNS = func(_ context.Context, _, host string, recs, have []store.CustomRecord, managed bool, _ []string) ([]store.CustomRecord, []string, error) {
		seen.managed, seen.recs = managed, recs
		if host == "shop.example.org" {
			return nil, nil, errs.Conflictf("shop.example.org is not in a managed domain: stackr does not manage example.org")
		}
		if len(have) > 0 {
			return have, nil, nil
		}
		var out []store.CustomRecord
		for _, r := range recs {
			r.ID = "id-" + r.Type
			out = append(out, r)
		}
		return out, []string{"TXT shop.acme.dev: added (1 other there stays)"}, nil
	}

	w.files["c1"] = autoFile("{host: shop.example.org, dns: managed}", "")
	p, err := w.f.Plan(ctx, w.dev.ID, w.release(t, "c1").ID, io.Discard)
	must(t, err)
	if !strings.Contains(strings.Join(p.Blockers, "|"), "tile api: domain shop.example.org: shop.example.org is not in a managed domain") || !seen.managed {
		t.Fatalf("blockers = %v", p.Blockers)
	}

	w.files["c2"] = autoFile(`{host: shop.acme.dev, records: [{type: TXT, value: "v=1"}, {name: _dmarc, type: TXT, value: "v=DMARC1"}]}`, "")
	r := w.release(t, "c2")
	p, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	if len(seen.recs) != 2 || seen.recs[1].Name != "_dmarc" || seen.managed {
		t.Errorf("CheckDNS saw %+v", seen.recs)
	}
	if !strings.Contains(changeNotes(p), "added (1 other there stays)") {
		t.Errorf("notes = %s", changeNotes(p))
	}
	ds, err := w.f.D.Domains.ListByTile(ctx, w.tileIn(t, w.dev, "api").ID)
	must(t, err)
	if len(ds) != 1 || len(ds[0].Records) != 2 {
		t.Fatalf("rows = %+v", ds)
	}
	w.running(w.tileIn(t, w.dev, "api"))
	again, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(again), "domain") {
		t.Errorf("re-plan = %s", kinds(again))
	}
	out, _, err := w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	if !strings.Contains(string(out), "records:") || !strings.Contains(string(out), "name: _dmarc") {
		t.Errorf("export lacks the records:\n%s", out)
	}

	w.files["c3"] = autoFile("{host: shop.acme.dev, dns: maybe}", "")
	p, err = w.f.Plan(ctx, w.dev.ID, w.release(t, "c3").ID, io.Discard)
	if err == nil && !strings.Contains(strings.Join(p.Blockers, "|"), "dns: is managed, or left out") {
		t.Errorf("dns: maybe = %v %v", p.Blockers, err)
	}
}

func changeNotes(p *Plan) string {
	var out []string
	for _, c := range p.Changes {
		out = append(out, c.Note)
	}
	return strings.Join(out, "|")
}
