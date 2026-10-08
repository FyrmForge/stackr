package orgplan_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func setup(t *testing.T) (*store.Store, *orgplan.Leaf, string) {
	t.Helper()
	st := servicetest.Store(t)
	return st, orgplan.New(st.OrgPlans), seedOrg(t, st, "acme")
}

func seedOrg(t *testing.T, st *store.Store, slug string) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{
		ID:        id,
		Name:      slug,
		Slug:      slug,
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func plan(t *testing.T, l *orgplan.Leaf, org, status string) store.OrgPlan {
	t.Helper()
	p, err := l.Create(ctx, store.OrgPlan{
		OrgID:   org,
		Commit:  "abc",
		Summary: "1 to add",
		Plan:    "{}",
		Status:  status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func conflict(t *testing.T, what string, err error) {
	t.Helper()
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("%s = %v, want a Conflict", what, err)
	}
}

func status(t *testing.T, l *orgplan.Leaf, id, want string) store.OrgPlan {
	t.Helper()
	p, err := l.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != want {
		t.Errorf("plan %s is %s, want %s", id[:8], p.Status, want)
	}
	return p
}

// Approve and reject only from pending; applied only after an approve; a
// second approve is refused; an apply failure is error with its message.
func TestStatusMachine(t *testing.T) {
	_, l, org := setup(t)

	if _, err := l.Create(ctx, store.OrgPlan{OrgID: org, Status: orgplan.Applied}); err == nil {
		t.Error("a new plan stored as applied")
	}

	a := plan(t, l, org, orgplan.Pending)
	_, err := l.SetStatus(ctx, a.ID, orgplan.Applied)
	conflict(t, "apply an unapproved plan", err)
	if p, err := l.Approve(ctx, a.ID, nil, false); err != nil || p.DecidedAt == nil || p.Status != orgplan.Pending {
		t.Fatalf("approve = %+v %v, want pending and decided", p, err)
	}
	_, err = l.Approve(ctx, a.ID, nil, false)
	conflict(t, "second approve", err)
	_, err = l.SetStatus(ctx, a.ID, orgplan.Rejected)
	conflict(t, "reject an approved plan", err)
	if _, err := l.SetStatus(ctx, a.ID, orgplan.Applied); err != nil {
		t.Fatal(err)
	}
	status(t, l, a.ID, orgplan.Applied)
	_, err = l.SetStatus(ctx, a.ID, orgplan.Applied)
	conflict(t, "apply twice", err)
	_, err = l.Approve(ctx, a.ID, nil, false)
	conflict(t, "approve an applied plan", err)
	_, err = l.SetError(ctx, a.ID, "boom")
	conflict(t, "fail an applied plan", err)

	b := plan(t, l, org, orgplan.Pending)
	if _, err := l.SetStatus(ctx, b.ID, orgplan.Rejected); err != nil {
		t.Fatal(err)
	}
	if p := status(t, l, b.ID, orgplan.Rejected); p.DecidedAt == nil {
		t.Error("a rejected plan has no decided_at")
	}
	_, err = l.Approve(ctx, b.ID, nil, false)
	conflict(t, "approve a rejected plan", err)

	c := plan(t, l, org, orgplan.Pending)
	if _, err := l.Approve(ctx, c.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SetError(ctx, c.ID, "stack shop: already exists"); err != nil {
		t.Fatal(err)
	}
	if p := status(t, l, c.ID, orgplan.Error); p.Error != "stack shop: already exists" {
		t.Errorf("error = %q", p.Error)
	}

	clean := plan(t, l, org, orgplan.Clean)
	_, err = l.Approve(ctx, clean.ID, nil, false)
	conflict(t, "approve a clean plan", err)
	if _, err := l.SetStatus(ctx, clean.ID, orgplan.Superseded); err == nil {
		t.Error("SetStatus took superseded")
	}
	if _, err := l.Get(ctx, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown plan = %v", err)
	}
}

// Storing a plan supersedes the org's older pending and clean rows in the
// same transaction; an approved row, an error row, a decided row and
// another org's rows stay. RejectPending leaves the approved row too.
func TestSupersede(t *testing.T) {
	st, l, org := setup(t)
	theirs := plan(t, l, seedOrg(t, st, "globex"), orgplan.Pending)

	pending := plan(t, l, org, orgplan.Pending)
	clean := plan(t, l, org, orgplan.Clean)
	approved := plan(t, l, org, orgplan.Pending)
	if _, err := l.Approve(ctx, approved.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	broken := plan(t, l, org, orgplan.Error)

	var fresh store.OrgPlan
	if err := st.Tx(ctx, func(tx store.Tx) error {
		var err error
		fresh, err = orgplan.New(tx.OrgPlans).Create(ctx, store.OrgPlan{
			OrgID:   org,
			Commit:  "def",
			Summary: "1 to change",
			Plan:    "{}",
			Status:  orgplan.Pending,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	status(t, l, pending.ID, orgplan.Superseded)
	status(t, l, clean.ID, orgplan.Superseded)
	status(t, l, approved.ID, orgplan.Pending)
	status(t, l, broken.ID, orgplan.Error)
	status(t, l, fresh.ID, orgplan.Pending)
	status(t, l, theirs.ID, orgplan.Pending)

	// a rolled-back store rolls its supersede back with it
	var lost store.OrgPlan
	boom := errors.New("boom")
	err := st.Tx(ctx, func(tx store.Tx) error {
		var err error
		lost, err = orgplan.New(tx.OrgPlans).Create(ctx, store.OrgPlan{OrgID: org, Plan: "{}", Status: orgplan.Clean})
		if err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	status(t, l, fresh.ID, orgplan.Pending)
	if _, err := l.Get(ctx, lost.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the rolled-back plan = %v, want not found", err)
	}

	ps, err := l.ForOrg(ctx, org, 2)
	if err != nil || len(ps) != 2 || ps[0].ID != fresh.ID || ps[1].ID != broken.ID {
		t.Errorf("ForOrg(2) = %+v %v, want the newest two, newest first", ps, err)
	}

	if err := l.RejectPending(ctx, org); err != nil {
		t.Fatal(err)
	}
	status(t, l, fresh.ID, orgplan.Rejected)
	status(t, l, approved.ID, orgplan.Pending)
}

// An approve stores the ticked removal keys and the confirm on the row; a
// plan stored fresh carries neither.
func TestApproveStoresTicks(t *testing.T) {
	_, l, org := setup(t)
	a := plan(t, l, org, orgplan.Pending)
	if a.Ticked == nil || len(a.Ticked) != 0 || a.Confirmed {
		t.Fatalf("fresh plan ticked %v confirmed %v", a.Ticked, a.Confirmed)
	}
	got, err := l.Approve(ctx, a.ID, []string{"route:a.io", "dest:old"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Confirmed || len(got.Ticked) != 2 {
		t.Errorf("approved = %+v", got)
	}
	back, err := l.Get(ctx, a.ID)
	if err != nil || !back.Confirmed || len(back.Ticked) != 2 || back.Ticked[0] != "route:a.io" {
		t.Errorf("stored = %+v, %v", back, err)
	}
	b := plan(t, l, org, orgplan.Pending)
	got, err = l.Approve(ctx, b.ID, nil, false)
	if err != nil || got.Ticked == nil || len(got.Ticked) != 0 || got.Confirmed {
		t.Errorf("bare approve = %+v, %v", got, err)
	}
}

// The server's plans run the same machine with one scope: a new plan
// supersedes the older undecided ones, ticks are stored, and nothing leaks
// to the org's table.
func TestServerPlans(t *testing.T) {
	st := servicetest.Store(t)
	s := orgplan.NewServer(st.ServerPlans)
	mk := func(status, source string) store.ServerPlan {
		t.Helper()
		p, err := s.Create(ctx, store.ServerPlan{Commit: "abc", Plan: "{}", Status: status, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := s.Create(ctx, store.ServerPlan{Source: "repo", Status: orgplan.Applied}); err == nil {
		t.Error("a new server plan stored as applied")
	}
	first := mk(orgplan.Pending, "repo")
	second := mk(orgplan.Pending, "local")
	if p, _ := s.Get(ctx, first.ID); p.Status != orgplan.Superseded {
		t.Errorf("first plan is %s, want superseded", p.Status)
	}
	if _, err := s.Approve(ctx, second.ID, []string{"route:a.io"}, true); err != nil {
		t.Fatal(err)
	}
	_, err := s.Approve(ctx, second.ID, nil, false)
	conflict(t, "second approve", err)
	third := mk(orgplan.Pending, "repo")
	if p, _ := s.Get(ctx, second.ID); p.Status != orgplan.Pending || p.DecidedAt == nil {
		t.Errorf("an approved plan was superseded: %+v", p)
	}
	if _, err := s.SetStatus(ctx, second.ID, orgplan.Applied); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Get(ctx, second.ID); len(p.Ticked) != 1 || !p.Confirmed || p.Source != "local" {
		t.Errorf("applied plan = %+v", p)
	}
	ps, err := s.Latest(ctx, 2)
	if err != nil || len(ps) != 2 || ps[0].ID != third.ID {
		t.Errorf("Latest(2) = %+v %v, want newest first", ps, err)
	}
	if err := s.RejectUndecided(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Get(ctx, third.ID); p.Status != orgplan.Rejected {
		t.Errorf("third plan is %s, want rejected", p.Status)
	}
	if n := orgPlanCount(t, st); n != 0 {
		t.Errorf("org_config_plans has %d rows from server plans", n)
	}
}

func orgPlanCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().Get(&n, "SELECT count(*) FROM org_config_plans"); err != nil {
		t.Fatal(err)
	}
	return n
}
