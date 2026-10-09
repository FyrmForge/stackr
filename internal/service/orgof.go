package service

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// Child is where a child row sits: its org, and the stack, env and tile it
// belongs to when it has them ("" = the row sits above that level).
type Child struct{ Org, Stack, Env, Tile string }

// OrgOf names the org a child row belongs to.
func (o *Orchestrator) OrgOf(ctx context.Context, kind, id string) (string, error) {
	c, err := o.ChildOf(ctx, kind, id)
	return c.Org, err
}

// ChildOf places a child row so the middleware can refuse an id from another
// org, or from another stack, env or tile than the route names, before a
// verb that takes ids on trust runs. kind is one of job, release, domain,
// volume, schedule, provision, domain-resource, org-plan. A row with no org
// (a panel job, an instance-level domain resource) answers Org "", which no
// org route accepts.
func (o *Orchestrator) ChildOf(ctx context.Context, kind, id string) (Child, error) {
	switch kind {
	case "job":
		j, err := o.jobRows.Get(ctx, id)
		if err != nil {
			return Child{}, err
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(j.Payload), &p)
		c := o.jobChild(ctx, p)
		c.Org, _ = p["org_id"].(string)
		return c, nil
	case "release":
		r, err := o.releases.Get(ctx, id)
		if err != nil {
			return Child{}, err
		}
		return o.scopeChild(ctx, "stack", r.StackID)
	case "domain":
		d, err := o.domains.Get(ctx, id)
		if err != nil {
			return Child{}, err
		}
		return o.tileChild(ctx, d.TileID)
	case "volume":
		return o.volumeChild(ctx, id)
	case "schedule":
		b, err := o.backups.GetSchedule(ctx, id)
		if err != nil {
			return Child{}, err
		}
		return o.volumeChild(ctx, b.VolumeID)
	case "provision":
		p, err := o.managed.GetProvision(ctx, id)
		if err != nil {
			return Child{}, err
		}
		return o.tileChild(ctx, p.TileID)
	case "domain-resource":
		r, err := o.domainres.Get(ctx, id)
		if err != nil {
			return Child{}, err
		}
		switch {
		case r.OrgID != nil:
			return Child{Org: *r.OrgID}, nil
		case r.StackID != nil:
			return o.scopeChild(ctx, "stack", *r.StackID)
		}
		return Child{}, nil
	case "org-plan":
		p, err := o.orgPlans.Get(ctx, id)
		if err != nil {
			return Child{}, err
		}
		return Child{Org: p.OrgID}, nil
	}
	return Child{}, errs.ErrNotFound
}

// jobChild places a job by the first id its payload names; a job about no
// stack (the org config jobs) answers an empty Child.
func (o *Orchestrator) jobChild(ctx context.Context, p map[string]any) Child {
	str := func(k string) string {
		s, _ := p[k].(string)
		return s
	}
	// The finest row the payload names; a tile or env deleted since falls
	// back to the stack_id stamped at enqueue.
	for _, try := range []func() (Child, error){
		func() (Child, error) { return o.tileChild(ctx, str("tile_id")) },
		func() (Child, error) { return o.scopeChild(ctx, "env", str("env_id")) },
		func() (Child, error) { return o.scopeChild(ctx, "stack", str("stack_id")) },
		func() (Child, error) { return o.volumeChild(ctx, str("volume_id")) },
	} {
		if c, err := try(); err == nil && c.Org != "" {
			return c
		}
	}
	return Child{}
}

func (o *Orchestrator) tileChild(ctx context.Context, id string) (Child, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return Child{}, err
	}
	c, err := o.scopeChild(ctx, "stack", t.StackID)
	c.Env, c.Tile = t.EnvironmentID, t.ID
	return c, err
}

func (o *Orchestrator) volumeChild(ctx context.Context, id string) (Child, error) {
	v, err := o.volumes.Get(ctx, id)
	if err != nil {
		return Child{}, err
	}
	return o.scopeChild(ctx, v.ScopeKind, v.ScopeID)
}

// scopeChild walks an org, env or stack scope up to its org.
func (o *Orchestrator) scopeChild(ctx context.Context, kind, id string) (Child, error) {
	switch kind {
	case "org":
		return Child{Org: id}, nil
	case "env":
		e, err := o.envs.Get(ctx, id)
		if err != nil {
			return Child{}, err
		}
		c, err := o.scopeChild(ctx, "stack", e.StackID)
		c.Env = e.ID
		return c, err
	}
	st, err := o.stacks.Get(ctx, id)
	return Child{Org: st.OrgID, Stack: st.ID}, err
}

// jobPlace reads the org and stack off a job payload's first id, at enqueue
// time, so a job outlives the tile or env it was about and still polls under
// its org and stack.
func (o *Orchestrator) jobPlace(ctx context.Context, p map[string]any) Child {
	str := func(k string) string {
		s, _ := p[k].(string)
		return s
	}
	var c Child
	var err error
	switch {
	case str("org_id") != "": // the org config jobs
		c = Child{Org: str("org_id")}
	case str("tile_id") != "":
		c, err = o.tileChild(ctx, str("tile_id"))
	case str("consumer_id") != "":
		c, err = o.tileChild(ctx, str("consumer_id"))
	case str("TileID") != "": // imagewatch.Scope
		c, err = o.tileChild(ctx, str("TileID"))
	case str("env_id") != "":
		c, err = o.scopeChild(ctx, "env", str("env_id"))
	case str("stack_id") != "":
		c, err = o.scopeChild(ctx, "stack", str("stack_id"))
	case str("StackID") != "":
		c, err = o.scopeChild(ctx, "stack", str("StackID"))
	case str("volume_id") != "":
		c, err = o.volumeChild(ctx, str("volume_id"))
	case str("target_volume_id") != "":
		c, err = o.volumeChild(ctx, str("target_volume_id"))
	case str("provision_id") != "":
		c, err = o.ChildOf(ctx, "provision", str("provision_id"))
	}
	if err != nil {
		return Child{}
	}
	return c
}

// withOrg stamps org_id and stack_id into a job payload.
func (o *Orchestrator) withOrg(ctx context.Context, b []byte) []byte {
	var p map[string]any
	if json.Unmarshal(b, &p) != nil || p == nil {
		return b
	}
	c := o.jobPlace(ctx, p)
	if c.Org == "" {
		return b
	}
	p["org_id"] = c.Org
	if c.Stack != "" {
		p["stack_id"] = c.Stack
	}
	out, err := json.Marshal(p)
	if err != nil {
		return b
	}
	return out
}
