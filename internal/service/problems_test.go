package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A failing scheduled backup opens a problem with no webhook set; a reminder
// keeps its since; the next good run clears it. The panel's the same.
func TestBackupFailureOpensProblem(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	for _, tt := range []struct{ key, subject, want string }{
		{"schedule.s1", "volume x", "Backup of volume x failing: s3 down"},
		{"panel", "the panel database", "Panel backup failing: s3 down"},
	} {
		o.backupResult(ctx, tt.key, "", tt.subject, errors.New("s3 down"))
		ps, err := o.Problems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ps) != 1 || ps[0].Message != tt.want || ps[0].Severity != SevCritical ||
			ps[0].Link != "/-/admin/backups" || ps[0].LinkText != "Open backups" {
			t.Fatalf("%s: %+v", tt.key, ps)
		}
		since := ps[0].Since
		must(t, o.store.Settings.Set(ctx, "alert.backup."+tt.key, time.Now().Add(-backupRemind-time.Hour).UTC().Format(time.RFC3339)))
		o.backupResult(ctx, tt.key, "", tt.subject, errors.New("s3 still down"))
		if ps, _ = o.Problems(ctx); len(ps) != 1 || !ps[0].Since.Equal(since) || !strings.HasSuffix(ps[0].Message, "s3 still down") {
			t.Errorf("%s reminder: %+v, since %v", tt.key, ps, since)
		}
		o.backupResult(ctx, tt.key, "", tt.subject, nil)
		if ps, _ = o.Problems(ctx); len(ps) != 0 {
			t.Errorf("%s after success: %+v", tt.key, ps)
		}
	}
}

// Problems lists an offline worker from its row, critical first, then
// oldest first; a row about a node that is gone is left out.
func TestProblemsOrder(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	must(t, o.store.Nodes.Create(ctx, store.Node{ID: "w1", Name: "h3", Slug: "h3", Role: node.Worker, Tags: "[]",
		Status: node.StatusOffline, CreatedAt: time.Now()}))
	o.openProblem(ctx, "alert.disk.w1", SevWarning, "h3 is low on disk.")
	o.openProblem(ctx, "alert.disk.gone", SevWarning, "a forgotten node.")
	o.openProblem(ctx, "alert.cert.root", SevCritical, "root cert.")
	ps, err := o.Problems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ps {
		got = append(got, p.Key+" "+p.Link)
	}
	want := []string{"node.w1 ?drawer=node:w1", "cert.root /-/admin/keys", "disk.w1 ?drawer=node:w1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v, want %v", got, want)
	}
	h3 := store.Node{ID: "w1", Name: "h3"}
	o.diskAlert(ctx, h3, 1<<20, 100<<30)  // low: marks the alert row
	o.diskAlert(ctx, h3, 50<<30, 100<<30) // recovered: the problem goes with it
	if ps, _ = o.Problems(ctx); len(ps) != 2 {
		t.Errorf("after clear: %+v", ps)
	}
}
