package service

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
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

// WaitingOnYou lists what waits on the principal: config plans to approve,
// releases behind the env below them, values a parked deploy needs, access
// and backup destinations an admin must approve, the server file's plan.
// Each group is only what their role lets them act on.
func (o *Orchestrator) WaitingOnYou(ctx context.Context, p *Principal) (Waiting, error) {
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
			for i, r := range rungs {
				if r.Behind {
					add("Releases to promote", WaitItem{
						Title: st.Name + " / " + r.Name + ", release " + strconv.Itoa(rungs[i-1].Release),
						Sub:   r.Name + " runs release " + strconv.Itoa(r.Release),
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
		if env == "" || !o.stillUnset(ctx, pj, env) {
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
			add("Access to approve", WaitItem{Title: st.Name, Sub: plural(n, "job") + waits(n) + " for access", Href: "/-/admin/access", Act: "Review"})
		} else if authz.Can(p.Access, "env.write", authz.Resource{OrgID: st.OrgID, StackID: st.ID}) == nil {
			w.Others = append(w.Others, WaitItem{Title: st.Name + ", access", Sub: plural(n, "job") + waits(n) + " for a server admin"})
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
