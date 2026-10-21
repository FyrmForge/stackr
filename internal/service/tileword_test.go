package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A failed cron run is amber on its header and its env card; three in a
// row are red. Its run jobs fail with it and must not override that.
func TestCronRunsRollUp(t *testing.T) {
	ctx := context.Background()
	e := servicetest.New(t)
	sh := e.Tile(t, e.Org(t, "acme"))
	cron := tileRow(sh.Stack, sh.Env, "nightly", "cron", func(t *store.Tile) { t.Schedule = "0 3 * * *" })
	if err := e.Store.Tiles.Create(ctx, cron); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	fail := func() {
		at = at.Add(time.Minute)
		jid := uuid.NewString()
		if err := e.Store.Jobs.Create(ctx, store.Job{
			ID: jid, Kind: "run", State: "failed", LockSet: store.StringList{cron.ID},
			Payload: "{}", CreatedAt: at, FinishedAt: &at,
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.Runs.Create(ctx, store.Run{
			ID: uuid.NewString(), TileID: cron.ID, JobID: jid, Trigger: "schedule",
			Status: "failed", CreatedAt: at, FinishedAt: &at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	words := func() (tile, env string) {
		st, err := e.Orch.TileStatus(ctx, cron.ID)
		if err != nil {
			t.Fatal(err)
		}
		v, err := e.Orch.Canvas(ctx, service.CanvasScope{Kind: service.CanvasStack, ID: sh.Stack}, service.ShowAll)
		if err != nil {
			t.Fatal(err)
		}
		return st.Word, nodes(v)["env:"+sh.Env].Status
	}

	if tw, _ := words(); tw != "idle" {
		t.Errorf("never run: header %q, want idle", tw)
	}
	fail()
	if tw, ew := words(); tw != "degraded" || ew != "degraded" {
		t.Errorf("one failure: header %q env %q, want degraded", tw, ew)
	}
	fail()
	fail()
	if tw, ew := words(); tw != "error" || ew != "error" {
		t.Errorf("three failures: header %q env %q, want error", tw, ew)
	}
}
