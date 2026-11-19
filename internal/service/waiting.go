package service

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
)

// WaitItem is one thing that needs a person: what, why, where to act, and
// the button's word.
type WaitItem struct{ Title, Sub, Href, Act string }

// WaitGroup is the items of one kind.
type WaitGroup struct {
	Title string
	Items []WaitItem
}

// Waiting is everything waiting on one person. Others is what waits on
// someone with more rights than they have: shown muted, never counted.
type Waiting struct {
	Groups []WaitGroup
	Others []WaitItem
}

// Count is the items the person can act on.
func (w Waiting) Count() int {
	n := 0
	for _, g := range w.Groups {
		n += len(g.Items)
	}
	return n
}

// waitCountTTL is how long a user's count serves the badge: it is drawn on
// every page and polled every 3 seconds, so it has to be younger than a poll
// for a change made elsewhere (a job ending, a push) to show on the next one;
// the cache only spares the page load and its banner fetch a second read.
const waitCountTTL = 2 * time.Second

// ForgetWaitingCounts drops every cached count, after a write that may have
// changed what waits on someone: the acting tab's next read is fresh.
func (o *Orchestrator) ForgetWaitingCounts() {
	o.waitCounts.mu.Lock()
	clear(o.waitCounts.m)
	o.waitCounts.mu.Unlock()
}

type waitCountCache struct {
	mu sync.Mutex
	m  map[string]waitCount
}

type waitCount struct {
	n  int
	at time.Time
}

// WaitingCount is len of what waits on the principal, for the shell's badge:
// the cheap path (no plan is computed) and at most waitCountTTL old. The
// Waiting page runs the full list.
func (o *Orchestrator) WaitingCount(ctx context.Context, p *Principal) int {
	c := &o.waitCounts
	c.mu.Lock()
	if e, ok := c.m[p.User.ID]; ok && time.Since(e.at) < waitCountTTL {
		c.mu.Unlock()
		return e.n
	}
	c.mu.Unlock()
	w, _ := o.waitingOnYou(ctx, p, false)
	n := w.Count()
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]waitCount{}
	}
	c.m[p.User.ID] = waitCount{n, time.Now()}
	c.mu.Unlock()
	return n
}

// WaitingOnYou lists what waits on the principal: config plans to approve,
// releases behind the env below them, values a parked deploy needs, access
// and backup destinations an admin must approve, the server file's plan.
// Each group is only what their role lets them act on.
func (o *Orchestrator) WaitingOnYou(ctx context.Context, p *Principal) (Waiting, error) {
	return o.waitingOnYou(ctx, p, true)
}

// waitingOnYou is WaitingOnYou; plans = false leaves out what only the
// rows' words need (a release's plan counts), which the count never reads.
func (o *Orchestrator) waitingOnYou(ctx context.Context, p *Principal, plans bool) (Waiting, error) {
	var w Waiting
	add := func(title string, it WaitItem) {
		if i := slices.IndexFunc(w.Groups, func(g WaitGroup) bool { return g.Title == title }); i >= 0 {
			w.Groups[i].Items = append(w.Groups[i].Items, it)
			return
		}
		w.Groups = append(w.Groups, WaitGroup{Title: title, Items: []WaitItem{it}})
	}
	// groups in the order the page draws them, whatever order they fill in
	defer func() {
		order := []string{"Plans to approve", "Releases to promote", "Values to set", "Access to approve", "Backup destinations to approve", "Server file plans"}
		rank := func(t string) int { return slices.Index(order, t) }
		slices.SortStableFunc(w.Groups, func(a, b WaitGroup) int { return rank(a.Title) - rank(b.Title) })
	}()

	var orgs []Org
	var err error
	if p.Access.Admin {
		orgs, err = o.orgs.ListAll(ctx)
	} else {
		orgs, err = o.orgs.ListForUser(ctx, p.User.ID)
	}
	if err != nil {
		return w, err
	}
	for _, og := range orgs {
		if authz.Can(p.Access, "orgplan.approve", authz.Resource{OrgID: og.ID}) == nil {
			if ps, err := o.orgPlans.ForOrg(ctx, og.ID, 1); err == nil && len(ps) == 1 && ps[0].Status == orgplan.Pending && ps[0].DecidedAt == nil {
				add("Plans to approve", WaitItem{
					Title: "Config file of " + og.Name,
					Sub:   ps[0].Summary + ", pushed " + ago(time.Since(ps[0].CreatedAt)),
					Href:  "/" + og.Slug + "?drawer=org%3A" + og.ID + "&tab=config", Act: "Review",
				})
			}
		}
		sts, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return w, err
		}
		for _, st := range sts {
			if authz.Can(p.Access, "env.write", authz.Resource{OrgID: og.ID, StackID: st.ID}) != nil {
				continue
			}
			rungs, err := o.graph.Rungs(ctx, st.ID)
			if err != nil {
				return w, err
			}
			for _, r := range rungs {
				if r.Behind {
					sub := ""
					if plans {
						sub = o.releaseWaitSub(ctx, p, st.ID, r)
					}
					add("Releases to promote", WaitItem{
						Title: st.Name + " / " + r.Name + ", release " + strconv.Itoa(r.Waiting),
						Sub:   sub,
						Href:  "/" + og.Slug + "/" + st.Slug + "/" + r.Slug + "?drawer=env%3A" + r.EnvID + "&tab=releases", Act: "Review plan",
					})
				}
			}
		}
	}
	if err := o.waitingParked(ctx, p, &w, add); err != nil {
		return w, err
	}
	if p.Access.Admin {
		gs, _ := o.DestGrants(ctx)
		for _, g := range gs {
			if g.Pending {
				add("Backup destinations to approve", WaitItem{Title: g.Name + " at " + g.Addr, Sub: "Asked by " + g.Org, Href: "/-/admin/access", Act: "Review"})
			}
		}
		if ps, err := o.serverPlans.Latest(ctx, 1); err == nil && len(ps) == 1 && ps[0].Status == orgplan.Pending && ps[0].DecidedAt == nil {
			add("Server file plans", WaitItem{Title: "Server file", Sub: ps[0].Summary + ", pushed " + ago(time.Since(ps[0].CreatedAt)), Href: "/-/admin/config", Act: "Review"})
		}
	}
	return w, nil
}

// releaseWaitSub is a release row's words: what promoting it would do, as
// the review's summary counts it ("2 to add, 1 to change"); the plain fact
// when the plan cannot be made.
func (o *Orchestrator) releaseWaitSub(ctx context.Context, p *Principal, stackID string, r GraphRung) string {
	fact := "release " + strconv.Itoa(r.Waiting) + " is not on " + r.Name
	rs, err := o.releases.List(ctx, stackID)
	if err != nil {
		return fact
	}
	i := slices.IndexFunc(rs, func(x Release) bool { return x.Number == r.Waiting })
	if i < 0 {
		return fact
	}
	pl, err := o.PlanPromote(ctx, p, r.EnvID, rs[i].ID)
	if err != nil {
		return fact
	}
	var parts []string
	add, change, remove := planCounts(pl.Plan.Changes)
	for _, c := range []struct {
		n int
		w string
	}{{add, "to add"}, {change, "to change"}, {remove, "to remove"}} {
		if c.n > 0 {
			parts = append(parts, strconv.Itoa(c.n)+" "+c.w)
		}
	}
	if len(parts) == 0 {
		return "nothing to change"
	}
	return strings.Join(parts, ", ")
}

// planCounts is the review's summary: rows that add, change and remove (the
// operated rows of a tile the plan creates fold into its create row).
func planCounts(cs []promote.Change) (add, change, remove int) {
	made := map[string]bool{}
	for _, c := range cs {
		if c.Kind == "create" {
			made[c.Tile] = true
		}
	}
	for _, c := range cs {
		switch {
		case c.Kind == "operated" && made[c.Tile]:
		case c.Optional || c.Kind == "delete" || c.Kind == "orphan" || c.Kind == "detach":
			remove++
		case c.Kind == "create":
			add++
		default:
			change++
		}
	}
	return
}

// waitingParked adds what the parked jobs wait for: a value (to a person who
// can write the env) or host access (to an admin; to a writer it waits on
// someone else).
func (o *Orchestrator) waitingParked(ctx context.Context, p *Principal, w *Waiting, add func(string, WaitItem)) error {
	pjs, err := o.parkedJobs(ctx)
	if err != nil {
		return err
	}
	js, err := o.jobRows.List(ctx, job.Waiting)
	if err != nil {
		return err
	}
	type key struct{ env, param string }
	vals := map[key][]string{} // tile slugs
	var order []key
	for _, pj := range pjs {
		var m map[string]any
		_ = json.Unmarshal([]byte(pj.Job.Payload), &m)
		env := o.jobEnv(ctx, m)
		if env == "" {
			continue
		}
		k := key{env, pj.Param}
		if _, ok := vals[k]; !ok {
			order = append(order, k)
			vals[k] = nil
		}
		ts := parkedTiles(pj.Job)
		if len(ts) == 0 {
			ts = []string{""}
		}
		for _, id := range ts {
			slug := "a tile"
			if t, err := o.tiles.Get(ctx, id); err == nil {
				slug = t.Slug
			}
			if !slices.Contains(vals[k], slug) {
				vals[k] = append(vals[k], slug)
			}
		}
	}
	access := map[string]int{} // stack -> jobs
	for _, j := range js {
		if j.WaitingParam == nil {
			continue
		}
		var pl struct {
			HostAccess *hostAccess `json:"host_access"`
		}
		_ = json.Unmarshal([]byte(j.Payload), &pl)
		if _, approval := hostgrant.Parse(*j.WaitingParam); approval && pl.HostAccess != nil && !o.stackGone(ctx, pl.HostAccess.Stack) {
			access[pl.HostAccess.Stack]++
		}
	}
	orgVals := map[key][]string{} // org param waits, one item per org and param: {org id, param}
	var orgOrder []key
	var orgSt = map[key]Stack{}
	for _, k := range order {
		e, err := o.envs.Get(ctx, k.env)
		if err != nil {
			continue
		}
		st, err := o.stacks.Get(ctx, e.StackID)
		if err != nil {
			continue
		}
		og, err := o.orgs.Get(ctx, st.OrgID)
		if err != nil {
			continue
		}
		if strings.HasPrefix(k.param, "org.") {
			ok := key{og.ID, k.param}
			if _, seen := orgVals[ok]; !seen {
				orgOrder, orgSt[ok] = append(orgOrder, ok), st
			}
			for _, sl := range vals[k] {
				if !slices.Contains(orgVals[ok], sl) {
					orgVals[ok] = append(orgVals[ok], sl)
				}
			}
			continue
		}
		if authz.Can(p.Access, "env.write", authz.Resource{OrgID: og.ID, StackID: st.ID}) != nil {
			continue
		}
		add("Values to set", WaitItem{
			Title: st.Name + " / " + e.Name + ", " + k.param,
			Sub:   joinWords(vals[k]) + waits(len(vals[k])),
			Href:  "/" + og.Slug + "/" + st.Slug + "/" + e.Slug + "?drawer=env%3A" + e.ID + "&tab=variables", Act: "Set",
		})
	}
	for _, k := range orgOrder {
		st := orgSt[k]
		og, err := o.orgs.Get(ctx, k.env)
		if err != nil {
			continue
		}
		sub := joinWords(orgVals[k]) + waits(len(orgVals[k]))
		switch {
		case authz.Can(p.Access, "org.write", authz.Resource{OrgID: og.ID}) == nil: // set in the org's variables, which takes an owner
			add("Values to set", WaitItem{Title: og.Name + ", " + k.param, Sub: sub, Href: "/" + og.Slug + "?drawer=org%3A" + og.ID + "&tab=variables", Act: "Set"})
		case authz.Can(p.Access, "env.write", authz.Resource{OrgID: og.ID, StackID: st.ID}) == nil:
			w.Others = append(w.Others, WaitItem{Title: og.Name + ", " + k.param, Sub: sub + " for an org owner"})
		}
	}
	for stackID, n := range access {
		st, err := o.stacks.Get(ctx, stackID)
		if err != nil {
			continue
		}
		if p.Access.Admin {
			add("Access to approve", WaitItem{Title: st.Name, Sub: plural(n, "tile") + waits(n) + " for access", Href: "/-/admin/access", Act: "Review"})
		} else if authz.Can(p.Access, "env.write", authz.Resource{OrgID: st.OrgID, StackID: st.ID}) == nil {
			w.Others = append(w.Others, WaitItem{Title: st.Name + ", access", Sub: plural(n, "tile") + waits(n) + " for a server admin"})
		}
	}
	return nil
}

// waits is " waits" for one subject, " wait" for more.
func waits(n int) string {
	if n == 1 {
		return " waits"
	}
	return " wait"
}

// joinWords is "a", "a and b", "a, b and c".
func joinWords(xs []string) string {
	slices.Sort(xs)
	if len(xs) < 2 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}
