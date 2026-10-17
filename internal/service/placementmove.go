package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/move"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Placement moves: repointing a placement moves every env on it to the new
// node, after CI-style checks; a pin change that moves an existing env goes
// through the same flow. The env moves are the ones in move.go (live, one
// job per env); a placement_moves row ties them together, and the placement
// keeps its old node until every env is on the new one.

const (
	auditPlacementMove = "placement.move"
	auditPlacementNode = "placement.node"

	pmRunning = "running"
	pmFailed  = "failed"
	pmDone    = "done"
)

// placementMoveMu serialises placementMoveSettle: the env moves of one
// repoint end on different jobs.
// ponytail: one global lock, a per-placement one if settles ever queue up.
var placementMoveMu sync.Mutex

// MoveEnvLine is one env a move takes along.
type MoveEnvLine struct {
	Path     string
	Volumes  int
	Bytes    int64  // -1 = unknown
	Downtime string // "about 20 s"
}

// PlacementMovePlan is the checks and impact of a move before it runs.
type PlacementMovePlan struct {
	Placement, From, To string // node names (admin) or placement names (env pin change)
	Restore             bool   // the source was forgotten: restore from backups
	Envs                []MoveEnvLine
	Checks              []planfile.Check
	OK                  bool
}

// PlacementMoveStatus is a running or finished repoint. Envs is every env
// of the placement, named by path, with its move when it has one.
type PlacementMoveStatus struct {
	State     string // running | failed | done
	From, To  string
	Done, All int
	Error     string
	Envs      []MoveStatus
}

// MoveNeeded is SetPin's refusal when the pin change moves envs: one plan
// per env, each's Placement its path.
type MoveNeeded struct{ Plans []PlacementMovePlan }

func (e *MoveNeeded) Error() string {
	paths := make([]string, len(e.Plans))
	for i, p := range e.Plans {
		paths[i] = p.Placement
	}
	return "this pin change moves " + strings.Join(paths, ", ") + "; confirm the move"
}

// ---- checks ----

// pmNames says how a check names a node: admins see node names, an org's
// owners see the placement the node stands for.
type pmNames struct {
	label func(nodeID string) string
	admin bool
}

func (o *Orchestrator) pmNodeNames(ctx context.Context) pmNames {
	name := o.nodeNamer(ctx)
	fs, _ := o.store.ForgottenNodes.List(ctx)
	return pmNames{admin: true, label: func(id string) string {
		if i := slices.IndexFunc(fs, func(f store.ForgottenNode) bool { return f.ID == id }); i >= 0 {
			return fs[i].Name
		}
		return name(id)
	}}
}

// pmForgotten: the node id is in forgotten_nodes.
func (o *Orchestrator) pmForgotten(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	fs, err := o.store.ForgottenNodes.List(ctx)
	return slices.ContainsFunc(fs, func(f store.ForgottenNode) bool { return f.ID == id }), err
}

// moveChecks are the CI-style checks of moving the envs from one node to
// another. fromNode "" is no node yet; a forgotten fromNode is a restore.
func (o *Orchestrator) moveChecks(ctx context.Context, envIDs []string, fromNode, toNode string) ([]planfile.Check, error) {
	return o.pmChecks(ctx, envIDs, fromNode, toNode, Needs{}, o.pmNodeNames(ctx))
}

// pmVolumes are the env's volumes, orphans excluded, read from the rows
// alone: a waiting env has no node to ask.
func (o *Orchestrator) pmVolumes(ctx context.Context, envID string) ([]store.Volume, error) {
	vs, err := o.store.Volumes.ListByScope(ctx, "env", envID)
	return slices.DeleteFunc(vs, func(v store.Volume) bool { return v.OrphanedAt != nil }), err
}

// pmEnvNeeds is what the envs ask of the node they land on (envNeeds, per
// env) plus what extra adds, with the volumes and tiles the checks size.
func (o *Orchestrator) pmEnvNeeds(ctx context.Context, envIDs []string, extra Needs) (needs Needs, vols []store.Volume, tiles []store.Tile, err error) {
	needs = extra
	for _, id := range envIDs {
		n, err := o.envNeeds(ctx, id)
		if err != nil {
			return needs, nil, nil, err
		}
		needs.Storage = needs.Storage || n.Storage
		vs, err := o.pmVolumes(ctx, id)
		if err != nil {
			return needs, nil, nil, err
		}
		ts, err := o.store.Tiles.ListByEnv(ctx, id)
		if err != nil {
			return needs, nil, nil, err
		}
		vols, tiles = append(vols, vs...), append(tiles, ts...)
	}
	return needs, vols, tiles, nil
}

func (o *Orchestrator) pmChecks(ctx context.Context, envIDs []string, from, to string, extra Needs, nm pmNames) ([]planfile.Check, error) {
	toNode, err := o.store.Nodes.Get(ctx, to)
	if err != nil {
		return nil, err
	}
	forgotten, err := o.pmForgotten(ctx, from)
	if err != nil {
		return nil, err
	}
	needs, vols, tiles, err := o.pmEnvNeeds(ctx, envIDs, extra)
	if err != nil {
		return nil, err
	}
	toName := nm.label(to)
	noSource := from == "" || forgotten
	toFacts := o.reportedFacts(ctx, toNode)
	var fromFacts *worker.Facts
	if !noSource {
		if fn, err := o.store.Nodes.Get(ctx, from); err == nil {
			fromFacts = o.reportedFacts(ctx, fn)
		}
	}

	var out []planfile.Check
	add := func(name string, ok bool, detail string) {
		out = append(out, planfile.Check{Name: name, OK: ok, Detail: detail})
	}

	// target online
	toUp := o.nodeUp(ctx, to) == nil
	if !toUp {
		add("target online", false, toName+" is offline")
	} else {
		add("target online", true, toName+" is online")
	}

	// mesh link: a measure of a link with an end down is stale
	switch {
	case noSource:
		add("mesh link", true, "no source node to reach")
	case !toUp:
		add("mesh link", false, "needs "+toName+" online")
	case o.nodeUp(ctx, from) != nil:
		add("mesh link", false, "needs "+nm.label(from)+" online")
	case o.mesh == nil:
		add("mesh link", true, "mesh not running")
	default:
		direct, judged := o.mesh.Direct(from, to)
		via := o.mesh.Via(from, to)
		switch {
		case !judged:
			add("mesh link", true, "link not measured yet")
		case direct:
			add("mesh link", true, "direct")
		case via != "":
			add("mesh link", true, "relayed through "+nm.label(via))
		default:
			add("mesh link", false, "no link between "+nm.label(from)+" and "+toName)
		}
	}

	// tags
	if miss := planfile.Fits(node.Tags(toNode), needs); len(miss) > 0 {
		add("tags", false, toName+" "+strings.Join(miss, ", "))
	} else {
		add("tags", true, "tags fit")
	}

	// disk
	ok, detail := o.pmDisk(ctx, from, to, from == "", forgotten, vols, nm)
	add("disk", ok, detail)

	// memory
	ok, detail = pmMemory(toName, toFacts, tiles)
	add("memory", ok, detail)

	// cpu arch
	switch {
	case noSource || fromFacts == nil || toFacts == nil || fromFacts.Arch == "" || toFacts.Arch == "":
		add("cpu arch", true, "arch not compared")
	case fromFacts.Arch != toFacts.Arch:
		add("cpu arch", false, fmt.Sprintf("%s is %s, %s is %s", nm.label(from), fromFacts.Arch, toName, toFacts.Arch))
	default:
		add("cpu arch", true, toFacts.Arch)
	}

	// source online
	switch {
	case from == "":
		add("source online", true, "no source node")
	case forgotten:
		add("source online", true, nm.label(from)+" is forgotten: restore from backups")
	default:
		if _, err := o.store.Nodes.Get(ctx, from); err != nil {
			add("source online", false, "the source node is gone")
		} else if o.nodeUp(ctx, from) != nil {
			msg := nm.label(from) + " is offline"
			if nm.admin {
				msg += ": wait for it, or forget it to restore from backups"
			} else {
				msg += ": wait for it"
			}
			add("source online", false, msg)
		} else {
			add("source online", true, nm.label(from)+" is online")
		}
	}
	return out, nil
}

// pmDisk: the target's free disk against the volumes' sizes plus 10%. A
// restore sizes each volume by its newest backup and fails on a volume that
// has none. A size or a disk that cannot be read is a detail, never a failure.
func (o *Orchestrator) pmDisk(ctx context.Context, from, to string, waiting, restore bool, vols []store.Volume, nm pmNames) (bool, string) {
	switch {
	case len(vols) == 0:
		return true, "no volumes"
	case waiting:
		return true, "no data yet: the envs wait for a node"
	}
	var total uint64
	unknown := 0
	var bare []string
	for _, v := range vols {
		if restore {
			r, err := o.newestBackup(ctx, v.ID)
			switch {
			case err != nil:
				return false, err.Error()
			case r == nil:
				bare = append(bare, v.Slug)
			case r.SizeBytes > 0:
				total += uint64(r.SizeBytes)
			default:
				unknown++
			}
			continue
		}
		info, err := o.docker.InspectVolume(docker.WithNode(ctx, from), v.Name)
		if err != nil || info.SizeBytes < 0 {
			unknown++
			continue
		}
		total += uint64(info.SizeBytes)
	}
	if len(bare) > 0 {
		return false, "no backup to restore of " + strings.Join(bare, ", ")
	}
	free, disk := o.diskOf(ctx, to)
	if free == 0 && disk == 0 {
		return true, nm.label(to) + " does not report its disk"
	}
	detail := fmt.Sprintf("%s free %s, needs %s + 10%%", nm.label(to), gb(free), gb(total))
	if unknown > 0 {
		detail += fmt.Sprintf(" (%d sizes unknown)", unknown)
	}
	return free >= total+total/10, detail
}

// pmMemory: the target's free memory against the tiles' limits. Tiles with
// no limit are a detail line.
func pmMemory(toName string, f *worker.Facts, tiles []store.Tile) (bool, string) {
	var mb uint64
	unlimited := 0
	for _, t := range tiles {
		if t.MemLimitMB > 0 {
			mb += uint64(t.MemLimitMB)
		} else {
			unlimited++
		}
	}
	note := ""
	if unlimited > 0 {
		note = fmt.Sprintf(" (%d tiles have no limit)", unlimited)
		if unlimited == 1 {
			note = " (1 tile has no limit)"
		}
	}
	switch {
	case mb == 0:
		return true, "no memory limits set" + note
	case f == nil || f.MemFree == 0:
		return true, toName + " does not report its memory" + note
	}
	need := mb << 20
	return f.MemFree >= need, fmt.Sprintf("%s free %s, needs %s%s", toName, gb(f.MemFree), gb(need), note)
}

// ---- plans ----

// pmDowntime is the heuristic shown in plans: 5 s plus 5 s per tile for a
// live move (it stops the env for the last sync and the start).
// ponytail: an estimate, not a measurement; weigh it by volume size if it misleads.
func pmDowntime(tiles int) string { return fmt.Sprintf("about %d s", 5+5*tiles) }

func (o *Orchestrator) pmEnvPath(ctx context.Context, e store.Environment) (string, error) {
	st, err := o.store.Stacks.Get(ctx, e.StackID)
	if err != nil {
		return "", err
	}
	og, err := o.store.Orgs.Get(ctx, st.OrgID)
	if err != nil {
		return "", err
	}
	return og.Slug + "/" + st.Slug + "/" + e.Slug, nil
}

// pmLine is one env's row of a plan; sizes are read on the env's node.
func (o *Orchestrator) pmLine(ctx context.Context, e store.Environment, restore bool) (MoveEnvLine, error) {
	path, err := o.pmEnvPath(ctx, e)
	if err != nil {
		return MoveEnvLine{}, err
	}
	vols, err := o.pmVolumes(ctx, e.ID)
	if err != nil {
		return MoveEnvLine{}, err
	}
	ts, err := o.store.Tiles.ListByEnv(ctx, e.ID)
	if err != nil {
		return MoveEnvLine{}, err
	}
	l := MoveEnvLine{Path: path, Volumes: len(vols), Downtime: pmDowntime(len(ts))}
	switch {
	case e.NodeID == "":
		l.Downtime = "none, it is waiting for a node"
	case restore:
		l.Bytes, l.Downtime = -1, "until restored from backups"
	default:
		for _, v := range vols {
			info, err := o.docker.InspectVolume(docker.WithNode(ctx, e.NodeID), v.Name)
			if err != nil || info.SizeBytes < 0 {
				l.Bytes = -1
				break
			}
			l.Bytes += info.SizeBytes
		}
	}
	return l, nil
}

func pmOK(cs []planfile.Check) bool {
	return !slices.ContainsFunc(cs, func(c planfile.Check) bool { return !c.OK })
}

// pmFailedChecks is the failed checks, one line.
func pmFailedChecks(cs []planfile.Check) string {
	var out []string
	for _, c := range cs {
		if !c.OK {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return strings.Join(out, "; ")
}

// pmMovers are the placement's envs that a repoint to nodeID touches: the
// ones not on it already (a nodeless env is given the node).
func pmMovers(es []store.Environment, nodeID string) []store.Environment {
	return slices.DeleteFunc(slices.Clone(es), func(e store.Environment) bool { return e.NodeID == nodeID })
}

// PlanPlacementMove is the checks and envs of repointing a placement (admin).
func (o *Orchestrator) PlanPlacementMove(ctx context.Context, placement, toNode string) (PlacementMovePlan, error) {
	p, err := o.store.Placements.GetByName(ctx, placement)
	if err != nil {
		return PlacementMovePlan{}, err
	}
	to, err := o.nodeByRef(ctx, toNode)
	if err != nil {
		return PlacementMovePlan{}, err
	}
	if p.NodeID == to.ID {
		return PlacementMovePlan{}, errs.Conflictf("%s is already on %s", p.Name, to.Name)
	}
	es, err := o.store.Environments.ListByPlacement(ctx, p.ID)
	if err != nil {
		return PlacementMovePlan{}, err
	}
	forgotten, err := o.pmForgotten(ctx, p.NodeID)
	if err != nil {
		return PlacementMovePlan{}, err
	}
	nm := o.pmNodeNames(ctx)
	plan := PlacementMovePlan{Placement: p.Name, To: to.Name, Restore: forgotten}
	if p.NodeID != "" {
		plan.From = nm.label(p.NodeID)
	}
	var ids []string
	for _, e := range pmMovers(es, to.ID) {
		l, err := o.pmLine(ctx, e, forgotten && e.NodeID == p.NodeID)
		if err != nil {
			return plan, err
		}
		plan.Envs = append(plan.Envs, l)
		ids = append(ids, e.ID)
	}
	extra, err := o.pmOverrideNeeds(ctx, p.ID)
	if err != nil {
		return plan, err
	}
	if plan.Checks, err = o.pmChecks(ctx, ids, p.NodeID, to.ID, extra, nm); err != nil {
		return plan, err
	}
	plan.OK = pmOK(plan.Checks)
	return plan, nil
}

// pmOverrideNeeds: a placement named as an ingress override needs an ingress
// node; one named in an exit ladder needs an egress node.
func (o *Orchestrator) pmOverrideNeeds(ctx context.Context, placementID string) (Needs, error) {
	var n Needs
	picks, err := o.store.IngressPicks.ListByPlacement(ctx, placementID)
	if err != nil {
		return n, err
	}
	rungs, err := o.store.ExitLadders.ListByTarget(ctx, placementID)
	if err != nil {
		return n, err
	}
	n.Ingress, n.Egress = len(picks) > 0, len(rungs) > 0
	return n, nil
}

// ---- repoint ----

// MovePlacement (admin) repoints a placement: confirm is its name. The checks
// run again; the job is not awaited. A placement with no node is given one
// (its waiting envs deploy), one on a forgotten node is restored from
// backups, one with no envs repoints at once; those return no job.
func (o *Orchestrator) MovePlacement(ctx context.Context, by *Principal, placement, toNode, confirm string) (Job, error) {
	if by == nil {
		return Job{}, errs.ErrRefused
	}
	if confirm != placement {
		return Job{}, errs.Invalidf("confirm", "type %s to confirm the move", placement)
	}
	plan, err := o.PlanPlacementMove(ctx, placement, toNode)
	if err != nil {
		return Job{}, err
	}
	if !plan.OK {
		return Job{}, errs.Conflictf("the checks failed: %s", pmFailedChecks(plan.Checks))
	}
	p, err := o.store.Placements.GetByName(ctx, placement)
	if err != nil {
		return Job{}, err
	}
	to, err := o.nodeByRef(ctx, toNode)
	if err != nil {
		return Job{}, err
	}
	if p.NodeID == "" {
		return Job{}, o.placementNodeSet(ctx, by, p.ID, to.ID)
	}
	if last, err := o.store.PlacementMoves.Latest(ctx, p.ID); err == nil && last.State == pmRunning {
		return Job{}, errs.Conflictf("a move of %s is already running", p.Name)
	} else if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return Job{}, err
	}
	if plan.Restore {
		return o.restorePlacement(ctx, by, p.ID, to.ID)
	}
	pm, err := o.pmBegin(ctx, by, p, to.ID, false)
	if err != nil {
		return Job{}, err
	}
	o.pmAudit(ctx, by, auditPlacementMove, "placement", p.ID, fmt.Sprintf("%s: %s -> %s", p.Name, plan.From, plan.To))
	return o.pmRun(ctx, by, pm)
}

func (o *Orchestrator) pmBegin(ctx context.Context, by *Principal, p store.Placement, toNode string, restore bool) (store.PlacementMove, error) {
	pm := store.PlacementMove{
		ID: uuid.NewString(), PlacementID: p.ID, FromNode: p.NodeID, ToNode: toNode, Restore: restore,
		State: pmRunning, By: by.User.ID, KeyID: by.KeyID, StartedAt: time.Now().UTC(),
	}
	return pm, o.store.PlacementMoves.Create(ctx, pm)
}

// restorePlacement is a repoint off a forgotten node (D8): MovePlacement
// calls it when the source is in forgotten_nodes. Each env is rehomed,
// deployed and restored from backups.
func (o *Orchestrator) restorePlacement(ctx context.Context, by *Principal, placementID, toNodeID string) (Job, error) {
	p, err := o.store.Placements.Get(ctx, placementID)
	if err != nil {
		return Job{}, err
	}
	if forgotten, err := o.pmForgotten(ctx, p.NodeID); err != nil {
		return Job{}, err
	} else if !forgotten {
		return Job{}, errs.Conflictf("%s is not on a forgotten node", p.Name)
	}
	pm, err := o.pmBegin(ctx, by, p, toNodeID, true)
	if err != nil {
		return Job{}, err
	}
	o.pmAudit(ctx, by, auditPlacementMove, "placement", p.ID, fmt.Sprintf("%s: restore onto %s", p.Name, o.nodeName(ctx, toNodeID)))
	return o.pmRun(ctx, by, pm)
}

// pmRun starts the env moves of a repoint that are still to do and returns
// the first job. An env that cannot start ends the repoint failed; the ones
// already queued carry on, and Retry takes the rest.
func (o *Orchestrator) pmRun(ctx context.Context, by *Principal, pm store.PlacementMove) (Job, error) {
	es, err := o.store.Environments.ListByPlacement(ctx, pm.PlacementID)
	if err != nil {
		return Job{}, err
	}
	var first Job
	var warn []error
	for _, e := range pmMovers(es, pm.ToNode) {
		if e.NodeID == "" {
			continue
		}
		j, err := o.pmStartEnv(ctx, by, pm, e, &warn)
		if err != nil {
			now := time.Now().UTC()
			_ = o.store.PlacementMoves.SetState(ctx, pm.ID, pmFailed, err.Error(), &now)
			return first, err
		}
		if first.ID == "" {
			first = j
		}
	}
	if err := o.placementMoveSettle(ctx, pm.ID, nil); err != nil {
		return first, err
	}
	if len(warn) > 0 {
		cur, err := o.store.PlacementMoves.Get(ctx, pm.ID)
		if err == nil {
			err = o.store.PlacementMoves.SetState(ctx, pm.ID, cur.State, errors.Join(warn...).Error(), cur.FinishedAt)
		}
		if err != nil {
			slog.Warn("placement move: restore notes not kept", "move", pm.ID, "err", err)
		}
	}
	return first, nil
}

// pmStartEnv restores an env of a forgotten node, or begins and queues the
// live move of any other.
func (o *Orchestrator) pmStartEnv(ctx context.Context, by *Principal, pm store.PlacementMove, e store.Environment, warn *[]error) (Job, error) {
	if forgotten, err := o.pmForgotten(ctx, e.NodeID); err != nil {
		return Job{}, err
	} else if forgotten {
		return o.pmRestoreEnv(ctx, by, e, pm.ToNode, warn)
	}
	m, err := o.moveFlow().Begin(ctx, move.Request{EnvID: e.ID, From: e.NodeID, To: pm.ToNode, Live: true})
	if err != nil {
		return Job{}, err
	}
	m.PlacementMoveID = &pm.ID
	if err := o.store.Moves.Update(ctx, m); err != nil {
		return Job{}, err
	}
	return o.enqueueMove(ctx, m, by.User.ID, by.KeyID)
}

// pmRestoreEnv rehomes a lost env on toNode, then restores its volumes from
// their newest backups and deploys its tiles (restoreThenDeploy). Failures
// and volumes that start empty are collected in warn: the env is on its node
// by then.
func (o *Orchestrator) pmRestoreEnv(ctx context.Context, by *Principal, e store.Environment, toNode string, warn *[]error) (Job, error) {
	if err := o.store.Environments.SetNode(ctx, e.ID, toNode); err != nil {
		return Job{}, err
	}
	o.pmAudit(ctx, by, auditEnvRestore, "env", e.ID, "to node "+o.nodeName(ctx, toNode))
	o.kickMesh()
	ts, err := o.tiles.List(ctx, e.ID)
	if err != nil {
		return Job{}, err
	}
	first, notes, err := o.restoreThenDeploy(ctx, e.ID, ts)
	for _, n := range notes {
		*warn = append(*warn, errors.New(n))
	}
	if err != nil {
		*warn = append(*warn, err)
	}
	return first, nil
}

// restoreThenDeploy brings an env's lost volumes back from their newest
// finished backups and queues a deploy of each of ts, so no app starts on an
// empty volume. The runner starts jobs whose locks overlap in the order they
// were queued, so the queue order is the mechanism: the tar restores first,
// each holding every tile of ts; then the deploys, dependencies first, with
// a managed instance's dump restore queued right after the instance's own
// deploy (a dump loads into a running engine; the restore holds the
// instance's consumers, which deploy after it). Only a volume that holds no
// data is restored, so one that kept its data is never rolled back. notes
// name the volumes that start empty or were left alone; every deploy's job
// log opens with them.
func (o *Orchestrator) restoreThenDeploy(ctx context.Context, envID string, ts []store.Tile) (Job, []string, error) {
	vs, err := o.store.Volumes.ListByScope(ctx, "env", envID)
	if err != nil {
		return Job{}, nil, err
	}
	hold := make([]string, 0, len(ts))
	for _, t := range ts {
		hold = append(hold, t.ID)
	}
	type dump struct {
		run string
		v   store.Volume
	}
	dumps := map[string][]dump{} // instance tile id -> its dump restores
	var notes []string
	var errl []error
	for _, v := range vs {
		if v.OrphanedAt != nil {
			continue
		}
		if o.volumes.HoldsData(ctx, v) {
			notes = append(notes, v.Slug+" holds data (or its node did not answer); not restored from a backup")
			continue
		}
		newest, err := o.newestBackup(ctx, v.ID)
		if err != nil {
			return Job{}, notes, err
		}
		if newest == nil {
			notes = append(notes, "no backup for "+v.Slug+"; it starts empty")
			continue
		}
		if strings.HasSuffix(newest.ObjectKey, ".sql.gz.age") && v.InstanceID != nil {
			it, err := o.instanceTile(ctx, *v.InstanceID)
			if err != nil {
				errl = append(errl, fmt.Errorf("restore %s: %w", v.Slug, err))
				continue
			}
			dumps[it.ID] = append(dumps[it.ID], dump{newest.ID, v})
			continue
		}
		// the pre-restore backup mounts it; made here, it carries stackr's label
		if _, err := o.volumes.Ensure(ctx, v); err != nil {
			errl = append(errl, fmt.Errorf("restore %s: %w", v.Slug, err))
			continue
		}
		if _, err := o.queueRestore(ctx, newest.ID, v, v, hold...); err != nil {
			errl = append(errl, fmt.Errorf("restore %s: %w", v.Slug, err))
		}
	}
	note := strings.Join(notes, "\n")
	var first Job
	for _, t := range deployOrder(ts) {
		j, err := o.deployNoted(ctx, t.ID, note)
		var down *NodeDownError
		switch {
		case errors.As(err, &down):
			slog.Warn("restore: deploy waits for the node", "tile", t.Slug, "node", down.Name)
		case err != nil:
			errl = append(errl, fmt.Errorf("deploy %s: %w", t.Slug, err))
		case first.ID == "":
			first = j
		}
		for _, d := range dumps[t.ID] {
			if _, err := o.queueRestore(ctx, d.run, d.v, d.v, hold...); err != nil {
				errl = append(errl, fmt.Errorf("restore %s: %w", d.v.Slug, err))
			}
		}
		delete(dumps, t.ID)
	}
	for _, ds := range dumps {
		for _, d := range ds {
			notes = append(notes, d.v.Slug+": its instance is not deployed, so its dump was not restored")
		}
	}
	return first, notes, errors.Join(errl...)
}

// newestBackup is the volume's newest finished backup run, nil when it has
// none. A pre-restore run is skipped: on a lost volume it is the empty one
// the restore itself took, not data.
func (o *Orchestrator) newestBackup(ctx context.Context, volumeID string) (*store.BackupRun, error) {
	runs, err := o.store.BackupRuns.ListByVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	var newest *store.BackupRun
	for i, r := range runs {
		if r.Status == backup.Done && r.Trigger != backup.PreRestore && (newest == nil || r.CreatedAt.After(newest.CreatedAt)) {
			newest = &runs[i]
		}
	}
	return newest, nil
}

// pmDeployEnv queues a deploy of every tile of the env, dependencies first,
// and returns the first job. A node that is down is not an error: the env
// waits there.
func (o *Orchestrator) pmDeployEnv(ctx context.Context, envID string) (Job, error) {
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return Job{}, err
	}
	var first Job
	var errl []error
	for _, t := range deployOrder(ts) {
		j, err := o.Deploy(ctx, t.ID)
		var down *NodeDownError
		switch {
		case errors.As(err, &down):
			slog.Warn("placement move: deploy waits for the node", "tile", t.Slug, "node", down.Name)
		case err != nil:
			errl = append(errl, fmt.Errorf("deploy %s: %w", t.Slug, err))
		case first.ID == "":
			first = j
		}
	}
	return first, errors.Join(errl...)
}

// placementMoveSettle ends a repoint when every env is on the target node
// (the placement takes the node), or marks it failed when rolled is an env
// move that rolled back.
func (o *Orchestrator) placementMoveSettle(ctx context.Context, pmID string, rolled *EnvMove) error {
	switched, err := o.placementMoveSettleLocked(ctx, pmID, rolled)
	if err != nil || !switched {
		return err
	}
	// ingress picks on the placement now resolve to its new node: push the
	// routes there now, not at the next sync (outside the lock: a push can wait)
	return o.sync.Sync(ctx)
}

// placementMoveSettleLocked is placementMoveSettle under placementMoveMu;
// switched is true when the placement took the target node.
func (o *Orchestrator) placementMoveSettleLocked(ctx context.Context, pmID string, rolled *EnvMove) (switched bool, err error) {
	placementMoveMu.Lock()
	defer placementMoveMu.Unlock()
	pm, err := o.store.PlacementMoves.Get(ctx, pmID)
	if err != nil || pm.State == pmDone {
		return false, err
	}
	es, err := o.store.Environments.ListByPlacement(ctx, pm.PlacementID)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	pending := slices.ContainsFunc(es, func(e store.Environment) bool { return e.NodeID != "" && e.NodeID != pm.ToNode })
	switch {
	case !pending:
		if err := o.store.Placements.SetNode(ctx, pm.PlacementID, pm.ToNode); err != nil {
			return false, err
		}
		o.kickMesh()
		return true, o.store.PlacementMoves.SetState(ctx, pm.ID, pmDone, "", &now)
	case rolled != nil && rolled.Phase == move.RolledBack && pm.State == pmRunning:
		return false, o.store.PlacementMoves.SetState(ctx, pm.ID, pmFailed, rolled.Error, &now)
	}
	return false, nil
}

// RetryPlacementMove (admin) moves the envs a failed repoint left behind.
func (o *Orchestrator) RetryPlacementMove(ctx context.Context, by *Principal, placement string) (Job, error) {
	if by == nil {
		return Job{}, errs.ErrRefused
	}
	p, err := o.store.Placements.GetByName(ctx, placement)
	if err != nil {
		return Job{}, err
	}
	pm, err := o.store.PlacementMoves.Latest(ctx, p.ID)
	if errors.Is(err, errs.ErrNotFound) {
		return Job{}, errs.Conflictf("%s has no move to retry", p.Name)
	}
	if err != nil {
		return Job{}, err
	}
	if pm.State != pmFailed {
		return Job{}, errs.Conflictf("the last move of %s is %s; nothing to retry", p.Name, pm.State)
	}
	es, err := o.store.Environments.ListByPlacement(ctx, p.ID)
	if err != nil {
		return Job{}, err
	}
	var ids []string
	for _, e := range pmMovers(es, pm.ToNode) {
		if e.NodeID != "" {
			ids = append(ids, e.ID)
		}
	}
	extra, err := o.pmOverrideNeeds(ctx, p.ID)
	if err != nil {
		return Job{}, err
	}
	cs, err := o.pmChecks(ctx, ids, pm.FromNode, pm.ToNode, extra, o.pmNodeNames(ctx))
	if err != nil {
		return Job{}, err
	}
	if !pmOK(cs) {
		return Job{}, errs.Conflictf("the checks failed: %s", pmFailedChecks(cs))
	}
	if err := o.store.PlacementMoves.SetState(ctx, pm.ID, pmRunning, "", nil); err != nil {
		return Job{}, err
	}
	pm.State = pmRunning
	return o.pmRun(ctx, by, pm)
}

// PlacementMove is the placement's latest move; nil = none.
func (o *Orchestrator) PlacementMove(ctx context.Context, placement string) (*PlacementMoveStatus, error) {
	p, err := o.store.Placements.GetByName(ctx, placement)
	if err != nil {
		return nil, err
	}
	pm, err := o.store.PlacementMoves.Latest(ctx, p.ID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	nm := o.pmNodeNames(ctx)
	s := &PlacementMoveStatus{State: pm.State, From: nm.label(pm.FromNode), To: nm.label(pm.ToNode), Error: pm.Error}
	es, err := o.store.Environments.ListByPlacement(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if e.NodeID == "" {
			continue
		}
		s.All++
		if e.NodeID == pm.ToNode {
			s.Done++
		}
		ms, err := o.EnvMoveStatus(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if ms == nil || ms.Move.PlacementMoveID == nil || *ms.Move.PlacementMoveID != pm.ID {
			// restored from backups, or not started: no move row of this repoint
			ms = &MoveStatus{Active: e.NodeID != pm.ToNode}
			ms.Path, _ = o.pmEnvPath(ctx, e)
		}
		s.Envs = append(s.Envs, *ms)
	}
	return s, nil
}

// ---- env pins (D4) ----

// pmPlacementNames labels nodes by the placements an org's owner may see.
func pmPlacementNames(fromID, fromName, toID, toName string) pmNames {
	return pmNames{label: func(id string) string {
		switch id {
		case fromID:
			return fromName
		case toID:
			return toName
		}
		return "another placement"
	}}
}

// PlanEnvPin is the move pinning an env to placement would cause; ""
// is the pin the env inherits. A pin that leaves the env on its node comes
// back with no Envs. Member-facing: it names placements, never nodes.
func (o *Orchestrator) PlanEnvPin(ctx context.Context, envID, placement string) (PlacementMovePlan, error) {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return PlacementMovePlan{}, err
	}
	var p store.Placement
	if placement == "" {
		p, err = o.effectivePlacement(ctx, e.StackID)
	} else {
		p, err = o.store.Placements.GetByName(ctx, placement)
	}
	if err != nil {
		return PlacementMovePlan{}, err
	}
	if p.NodeID == "" {
		return PlacementMovePlan{}, errs.Conflictf("placement %s has no node yet", p.Name)
	}
	path, err := o.pmEnvPath(ctx, e)
	if err != nil {
		return PlacementMovePlan{}, err
	}
	fromName := "the current placement"
	if e.PlacementID != "" {
		if cur, err := o.store.Placements.Get(ctx, e.PlacementID); err == nil {
			fromName = cur.Name
		}
	}
	plan := PlacementMovePlan{Placement: path, From: fromName, To: p.Name, OK: true}
	if e.NodeID == p.NodeID {
		return plan, nil
	}
	if plan.Restore, err = o.pmForgotten(ctx, e.NodeID); err != nil {
		return plan, err
	}
	l, err := o.pmLine(ctx, e, plan.Restore)
	if err != nil {
		return plan, err
	}
	plan.Envs = []MoveEnvLine{l}
	nm := pmPlacementNames(e.NodeID, fromName, p.NodeID, p.Name)
	if plan.Checks, err = o.pmChecks(ctx, []string{e.ID}, e.NodeID, p.NodeID, Needs{}, nm); err != nil {
		return plan, err
	}
	plan.OK = pmOK(plan.Checks)
	return plan, nil
}

// MoveEnvToPlacement pins an env and moves it: confirm is the env slug. A
// pin that leaves the env on its node just records it. An env with no node
// is given the target's and deployed; one on a forgotten node is restored
// from backups; any other is moved live as a job.
func (o *Orchestrator) MoveEnvToPlacement(ctx context.Context, by *Principal, envID, placement, confirm string) (Job, error) {
	if by == nil {
		return Job{}, errs.ErrRefused
	}
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return Job{}, err
	}
	plan, err := o.PlanEnvPin(ctx, envID, placement)
	if err != nil {
		return Job{}, err
	}
	p, err := o.store.Placements.GetByName(ctx, placement)
	if err != nil {
		return Job{}, err
	}
	if len(plan.Envs) == 0 {
		return Job{}, o.store.Environments.SetPlacement(ctx, e.ID, p.ID)
	}
	if confirm != e.Slug {
		return Job{}, errs.Invalidf("confirm", "type %s to confirm the move", e.Slug)
	}
	if !plan.OK {
		return Job{}, errs.Conflictf("the checks failed: %s", pmFailedChecks(plan.Checks))
	}
	o.pmAudit(ctx, by, auditPlacementMove, "env", e.ID, fmt.Sprintf("pin %s: %s -> %s restore=%t", plan.Placement, plan.From, plan.To, plan.Restore))
	switch {
	case e.NodeID == "":
		if err := o.store.Environments.SetPlacement(ctx, e.ID, p.ID); err != nil {
			return Job{}, err
		}
		return o.pmEnvNodeSet(ctx, e.ID, p.NodeID)
	case plan.Restore:
		if err := o.store.Environments.SetPlacement(ctx, e.ID, p.ID); err != nil {
			return Job{}, err
		}
		var warn []error
		j, err := o.pmRestoreEnv(ctx, by, e, p.NodeID, &warn)
		return j, errors.Join(append([]error{err}, warn...)...)
	}
	m, err := o.moveFlow().Begin(ctx, move.Request{EnvID: e.ID, From: e.NodeID, To: p.NodeID, Live: true})
	if err != nil {
		return Job{}, err
	}
	if err := o.store.Environments.SetPlacement(ctx, e.ID, p.ID); err != nil {
		return Job{}, err
	}
	return o.enqueueMove(ctx, m, by.User.ID, by.KeyID)
}

// ---- nodeless placements (D6) ----

// pmEnvNodeSet makes nodeID a waiting env's node and queues its deploys.
func (o *Orchestrator) pmEnvNodeSet(ctx context.Context, envID, nodeID string) (Job, error) {
	if err := o.store.Environments.SetNode(ctx, envID, nodeID); err != nil {
		return Job{}, err
	}
	o.kickMesh()
	return o.pmDeployEnv(ctx, envID)
}

// placementNodeSet gives a nodeless placement a node and deploys its waiting envs (D6).
func (o *Orchestrator) placementNodeSet(ctx context.Context, by *Principal, placementID, nodeID string) error {
	p, err := o.store.Placements.Get(ctx, placementID)
	if err != nil {
		return err
	}
	if p.NodeID != "" {
		return errs.Conflictf("%s already has a node; move it instead", p.Name)
	}
	n, err := o.store.Nodes.Get(ctx, nodeID)
	if err != nil {
		return err
	}
	if err := o.store.Placements.SetNode(ctx, p.ID, n.ID); err != nil {
		return err
	}
	o.pmAudit(ctx, by, auditPlacementNode, "placement", p.ID, p.Name+" on "+n.Name)
	o.kickMesh()
	if err := o.sync.Sync(ctx); err != nil { // ingress picks on it resolve to n now
		slog.Warn("placement node: route push", "placement", p.Name, "err", err)
	}
	es, err := o.store.Environments.ListByPlacement(ctx, p.ID)
	if err != nil {
		return err
	}
	var errl []error
	for _, e := range es {
		if e.NodeID != "" {
			continue
		}
		if _, err := o.pmEnvNodeSet(ctx, e.ID, n.ID); err != nil {
			errl = append(errl, err)
		}
	}
	o.kickMesh()
	return errors.Join(errl...)
}

func (o *Orchestrator) pmAudit(ctx context.Context, by *Principal, action, kind, id, detail string) {
	var actor, key string
	if by != nil {
		actor, key = by.User.ID, by.KeyID
	}
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{Action: action, ActorID: actor, KeyID: key, TargetKind: kind, TargetID: id, Detail: detail})
	})
	if err != nil {
		slog.Warn("placement move: audit row not written", "action", action, "target", id, "err", err)
	}
}
