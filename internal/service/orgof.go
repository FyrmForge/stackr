package service

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// OrgOf names the org a child row belongs to, so the middleware can refuse
// an id from another org before a verb that takes ids on trust runs. kind is
// one of job, release, domain, volume, schedule, provision,
// domain-resource, org-plan. A row with no org (a panel job, an instance-level domain
// resource) answers "", which no org route accepts.
func (o *Orchestrator) OrgOf(ctx context.Context, kind, id string) (string, error) {
	switch kind {
	case "job":
		j, err := o.jobRows.Get(ctx, id)
		if err != nil {
			return "", err
		}
		var p struct {
			OrgID string `json:"org_id"`
		}
		_ = json.Unmarshal([]byte(j.Payload), &p)
		return p.OrgID, nil
	case "release":
		r, err := o.releases.Get(ctx, id)
		if err != nil {
			return "", err
		}
		return o.scopeOrg(ctx, "stack", r.StackID)
	case "domain":
		d, err := o.domains.Get(ctx, id)
		if err != nil {
			return "", err
		}
		return o.tileOrg(ctx, d.TileID)
	case "volume":
		return o.volumeIDOrg(ctx, id)
	case "schedule":
		b, err := o.backups.GetSchedule(ctx, id)
		if err != nil {
			return "", err
		}
		return o.volumeIDOrg(ctx, b.VolumeID)
	case "provision":
		p, err := o.managed.GetProvision(ctx, id)
		if err != nil {
			return "", err
		}
		return o.tileOrg(ctx, p.TileID)
	case "domain-resource":
		r, err := o.domainres.Get(ctx, id)
		if err != nil {
			return "", err
		}
		switch {
		case r.OrgID != nil:
			return *r.OrgID, nil
		case r.StackID != nil:
			return o.scopeOrg(ctx, "stack", *r.StackID)
		}
		return "", nil
	case "org-plan":
		p, err := o.orgPlans.Get(ctx, id)
		if err != nil {
			return "", err
		}
		return p.OrgID, nil
	}
	return "", errs.ErrNotFound
}

func (o *Orchestrator) tileOrg(ctx context.Context, id string) (string, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return o.scopeOrg(ctx, "stack", t.StackID)
}

func (o *Orchestrator) volumeIDOrg(ctx context.Context, id string) (string, error) {
	v, err := o.volumes.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return o.volumeOrg(ctx, v)
}

// jobOrg reads the org off a job payload's first id, at enqueue time, so a
// job outlives the tile or env it was about and still polls under its org.
func (o *Orchestrator) jobOrg(ctx context.Context, p map[string]any) string {
	str := func(k string) string {
		s, _ := p[k].(string)
		return s
	}
	var org string
	var err error
	switch {
	case str("org_id") != "": // the org config jobs
		org = str("org_id")
	case str("tile_id") != "":
		org, err = o.tileOrg(ctx, str("tile_id"))
	case str("consumer_id") != "":
		org, err = o.tileOrg(ctx, str("consumer_id"))
	case str("TileID") != "": // imagewatch.Scope
		org, err = o.tileOrg(ctx, str("TileID"))
	case str("env_id") != "":
		org, err = o.scopeOrg(ctx, "env", str("env_id"))
	case str("stack_id") != "":
		org, err = o.scopeOrg(ctx, "stack", str("stack_id"))
	case str("StackID") != "":
		org, err = o.scopeOrg(ctx, "stack", str("StackID"))
	case str("volume_id") != "":
		org, err = o.volumeIDOrg(ctx, str("volume_id"))
	case str("target_volume_id") != "":
		org, err = o.volumeIDOrg(ctx, str("target_volume_id"))
	case str("provision_id") != "":
		org, err = o.OrgOf(ctx, "provision", str("provision_id"))
	}
	if err != nil {
		return ""
	}
	return org
}

// withOrg stamps org_id into a job payload.
func (o *Orchestrator) withOrg(ctx context.Context, b []byte) []byte {
	var p map[string]any
	if json.Unmarshal(b, &p) != nil || p == nil {
		return b
	}
	org := o.jobOrg(ctx, p)
	if org == "" {
		return b
	}
	p["org_id"] = org
	out, err := json.Marshal(p)
	if err != nil {
		return b
	}
	return out
}
