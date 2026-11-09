package service

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
)

// Synchronous replication of operated postgres. An instance that prefers it
// has each standby in a mode: auto (the primary waits for it while its round
// trip is under the instance's limit), always (it waits whatever the latency)
// or never. The standbys waited for go into synchronous_standby_names as
// ANY 1 (...); the primary then holds a commit until one of them has it.
//
// A fast loop (startSyncWatch, every few seconds) and the minute tick both
// run syncInstance, which decides (syncCtl.step, pure) and writes the setting.
// When no waited-for standby answers, "preferred" (the default) drops the
// primary to asynchronous after a short grace and alerts; "required" keeps
// waiting, and writes hang until one is back. The panel does the switch: with
// the panel down and the standby down too, writes hang (required always,
// preferred until the panel is back).

const (
	SyncAuto   = "auto"
	SyncAlways = "always"
	SyncNever  = "never"

	// DefaultSyncLimitMS is the round trip an auto standby may have.
	DefaultSyncLimitMS = 10

	syncHold     = 3 * time.Minute  // clearly under or over the limit this long before a standby moves in or out
	syncGrace    = 5 * time.Second  // no waited-for standby answering this long before the fallback
	syncReplyMax = 25               // a standby that has not replied for this many seconds is not answering (it replies every 10 when idle)
	syncEvery    = 5 * time.Second  // the fast loop
	syncRound    = 20 * time.Second // one pass over the instances
)

// Protection of the writes, as syncCtl.step reports it.
const (
	SyncOff         = "off"         // not preferring synchronous
	SyncProtected   = "protected"   // a commit waits for a standby
	SyncUnprotected = "unprotected" // preferring, but nothing is waited for
	SyncBlocked     = "blocked"     // required, no standby answers: writes hang
)

// AlertPGSync is the webhook event when the protection of an instance's
// writes changes; its problem key is pgmon.sync.<instance id>.
const AlertPGSync = "pgmon.sync"

// SyncModeOK says whether m is a sync mode.
func SyncModeOK(m string) bool { return m == SyncAuto || m == SyncAlways || m == SyncNever }

// syncMember is one standby as the decision sees it.
type syncMember struct {
	Slot      string
	Mode      string
	RTT       float64 // ms to the primary's node
	Measured  bool    // RTT is known
	Reachable bool    // it answers the primary now
	In        bool    // the primary waits for it now
}

type syncPolicy struct {
	On, Required bool
	LimitMS      float64
}

// syncCtl is one instance's memory between passes: when a standby began to
// look clearly under or over the limit, and since when no waited-for standby
// answers. Fresh (after a failover: the node pairs changed) skips the hold
// once. Last is the verdict of the newest pass.
type syncCtl struct {
	flip      map[string]time.Time
	downSince time.Time
	fresh     bool
	last      syncVerdict
	seen      bool
}

type syncVerdict struct {
	In         []string // the slots the primary waits for, sorted
	Names      []string // what goes into synchronous_standby_names (In, or none after the fallback)
	Protection string
}

// step is the whole policy: which standbys are in, what the primary waits
// for and how protected the writes are.
func (c *syncCtl) step(now time.Time, p syncPolicy, ms []syncMember) syncVerdict {
	if c.flip == nil {
		c.flip = map[string]time.Time{}
	}
	if !p.On {
		clear(c.flip)
		c.downSince = time.Time{}
		return syncVerdict{Protection: SyncOff}
	}
	var in []string
	reachable := 0
	for _, m := range ms {
		want := m.In
		switch m.Mode {
		case SyncNever:
			want = false
			delete(c.flip, m.Slot)
		case SyncAlways:
			want = true
			delete(c.flip, m.Slot)
		default: // auto
			under := m.RTT <= p.LimitMS
			if !m.Measured || under == m.In { // no round trip to go by, or nothing to change
				delete(c.flip, m.Slot)
				break
			}
			since, ok := c.flip[m.Slot]
			if !ok {
				c.flip[m.Slot], since = now, now
			}
			if c.fresh || now.Sub(since) >= syncHold {
				want = under
				delete(c.flip, m.Slot)
			}
		}
		if want {
			in = append(in, m.Slot)
			if m.Reachable {
				reachable++
			}
		}
	}
	if len(ms) > 0 {
		c.fresh = false // a pass with no standby to judge (the old primary still rewinding) leaves it for the next
	}
	slices.Sort(in)
	v := syncVerdict{In: in, Names: in, Protection: SyncProtected}
	switch {
	case len(in) == 0:
		v.Protection = SyncUnprotected
		c.downSince = time.Time{}
	case reachable > 0:
		c.downSince = time.Time{}
	default:
		if c.downSince.IsZero() {
			c.downSince = now
		}
		if now.Sub(c.downSince) >= syncGrace {
			if p.Required {
				v.Protection = SyncBlocked
			} else {
				v.Names, v.Protection = nil, SyncUnprotected
			}
		}
	}
	return v
}

func (c *syncCtl) remember(v syncVerdict) { c.last, c.seen = v, true }

// syncCtlOf is the instance's memory; the caller holds its topology lock.
func (o *Orchestrator) syncCtlOf(instanceID string) *syncCtl {
	v, _ := o.syncCtls.LoadOrStore(instanceID, &syncCtl{})
	return v.(*syncCtl)
}

// syncWords is the drawer's plain answer to "are my writes protected now".
func syncWords(prot string, standbys int) string {
	switch prot {
	case SyncProtected:
		return "Writes are protected: a commit waits for a standby."
	case SyncUnprotected:
		if standbys == 0 {
			return "Writes are not protected: there is no standby to wait for."
		}
		return "Writes are not protected right now: no standby is in sync."
	case SyncBlocked:
		return "Writes are frozen: no standby answers, and this instance requires one."
	}
	return "Writes do not wait for a standby."
}

var errSyncBusy = fmt.Errorf("the instance's topology is changing")

// nodeRTT is the mesh round trip in ms between two nodes; a pair on one node
// is 0. ok is false when neither end measured the other.
func (o *Orchestrator) nodeRTT(ctx context.Context, a, b string) (float64, bool) {
	if a == b {
		return 0, true
	}
	na, err := o.store.Nodes.Get(ctx, a)
	if err != nil {
		return 0, false
	}
	nb, err := o.store.Nodes.Get(ctx, b)
	if err != nil {
		return 0, false
	}
	fa, _, _ := o.clusterFacts(ctx, na)
	fb, _, _ := o.clusterFacts(ctx, nb)
	return pingMS(clusterNodeIn{Node: na, Facts: fa}, clusterNodeIn{Node: nb, Facts: fb})
}

// syncInstance is one pass for an instance: decide, write the setting (and
// the rest of stackr-owned.conf), keep the members' flags and the alert. It
// skips an instance whose topology is changing or that is failing over.
func (o *Orchestrator) syncInstance(ctx context.Context, t Tile, inst OperatedInstance) error {
	mu := o.topoLock(inst.ID)
	if !mu.TryLock() {
		return errSyncBusy
	}
	defer mu.Unlock()
	if o.failoverActive(ctx, inst.ID) {
		return errSyncBusy
	}
	return o.syncLocked(ctx, t, inst, false)
}

// syncLocked is syncInstance with the topology lock held. fresh skips the
// hold (after a failover).
func (o *Orchestrator) syncLocked(ctx context.Context, t Tile, inst OperatedInstance, fresh bool) error {
	inst, err := o.operated.Get(ctx, inst.ID) // the row as it is now
	if err != nil {
		return err
	}
	ctl := o.syncCtlOf(inst.ID)
	if fresh {
		ctl.fresh = true
	}
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil {
		return err
	}
	var sbs []InstanceMember
	for _, m := range ms {
		if m.Role == opmember.Replica && m.State != opmember.Removing && !m.Fenced && m.Phase != opmember.Rebuild && m.Phase != opmember.Rewind {
			sbs = append(sbs, m)
		}
	}
	var in []syncMember
	if inst.PreferSync && len(sbs) > 0 {
		states, err := o.engines.ReplicaStates(ctx, t) // the primary cannot be asked: change nothing
		if err != nil {
			return err
		}
		running := map[string]bool{}
		for _, c := range o.runningStandbys(ctx, inst.ID) {
			running[c.m.ID] = true
		}
		pnode, _ := o.primaryNode(ctx, t)
		for _, m := range sbs {
			slot := mflow.SlotName(m.ID)
			sm := syncMember{Slot: slot, Mode: m.SyncMode, In: m.SyncIn}
			sm.RTT, sm.Measured = o.nodeRTT(ctx, pnode, m.NodeID)
			for _, s := range states {
				if s.Name == slot && s.Streaming && s.ReplyAge >= 0 && s.ReplyAge <= syncReplyMax {
					sm.Reachable = running[m.ID]
				}
			}
			in = append(in, sm)
		}
	}
	limit := float64(inst.SyncLimitMS)
	if limit <= 0 {
		limit = DefaultSyncLimitMS
	}
	v := ctl.step(time.Now(), syncPolicy{On: inst.PreferSync, Required: inst.SyncRequired, LimitMS: limit}, in)
	ctl.remember(v)
	for _, m := range ms { // what the primary waits for is kept, for a promote to prefer
		_, err := o.members.SetSync(ctx, m, cmpMode(m.SyncMode), m.Role == opmember.Replica && slices.Contains(v.In, mflow.SlotName(m.ID)))
		if err != nil {
			slog.Warn("sync: member flag", "member", m.ID, "err", err)
		}
	}
	err = o.engines.SyncOwned(ctx, t, mflow.SyncStandbyNames(v.Names))
	o.syncAlert(ctx, t, inst, v, len(sbs))
	return err
}

func cmpMode(m string) string {
	if m == "" {
		return SyncAuto
	}
	return m
}

// syncAlert opens or clears the problem and sends the webhook when the
// protection of the instance's writes changed since the last pass (the
// settings row keeps it, so a restart does not repeat it).
func (o *Orchestrator) syncAlert(ctx context.Context, t Tile, inst OperatedInstance, v syncVerdict, standbys int) {
	key := "alert.pgmon.sync." + inst.ID
	prev, _, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	if v.Protection == SyncOff {
		o.clearProblem(ctx, key)
		if prev != "" {
			_ = o.store.Settings.Delete(ctx, key)
		}
		return
	}
	msg := t.Slug + ": " + syncWords(v.Protection, standbys)
	if v.Protection == SyncProtected {
		o.clearProblem(ctx, key)
	} else {
		sev := SevWarning
		if v.Protection == SyncBlocked {
			sev = SevCritical
		}
		o.openProblem(ctx, key, sev, msg)
	}
	if prev == v.Protection || o.store.Settings.Set(ctx, key, v.Protection) != nil {
		return
	}
	if prev == "" && v.Protection == SyncProtected {
		return // switched on and working: no news
	}
	sev := SevInfo
	switch v.Protection {
	case SyncUnprotected:
		sev = SevWarning
	case SyncBlocked:
		sev = SevCritical
	}
	o.alert(Alert{Event: AlertPGSync, Severity: sev, Subject: t.Slug, Message: msg})
}

// startSyncWatch runs the fast loop until the panel stops: every few seconds
// one pass over each instance that prefers synchronous, so a standby that
// goes away does not hold the primary's writes for a whole watch tick.
func (o *Orchestrator) startSyncWatch() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-o.stopNodes
		cancel()
	}()
	o.alertsDone.Add(1)
	go func() {
		defer o.alertsDone.Done()
		tk := time.NewTicker(syncEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				o.syncAll(ctx)
			}
		}
	}()
}

func (o *Orchestrator) syncAll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, syncRound)
	defer cancel()
	ms, err := o.operated.WithEngine(ctx, "postgres")
	if err != nil {
		return
	}
	for _, inst := range ms {
		if !inst.PreferSync {
			continue
		}
		t, err := o.tiles.Get(ctx, inst.TileID)
		if err != nil {
			continue
		}
		if err := o.syncInstance(ctx, t, inst); err != nil && err != errSyncBusy {
			slog.Debug("sync: pass", "instance", inst.ID, "err", err)
		}
	}
}

// syncAfterTopology recomputes the instance's setting with the topology lock
// held, after a removal or a failover changed which slots exist, so the
// primary never waits for one that is gone. Best effort: the loop follows.
func (o *Orchestrator) syncAfterTopology(ctx context.Context, t Tile, instanceID string, fresh bool) {
	inst, err := o.operated.Get(ctx, instanceID)
	if err != nil || !inst.PreferSync && !fresh {
		return
	}
	if err := o.syncLocked(ctx, t, inst, fresh); err != nil {
		slog.Warn("sync: not recomputed", "instance", instanceID, "err", err)
	}
}

// ---- the user's side ----

// SyncStandby is a standby's part of the sync view.
type SyncStandby struct {
	ID   string `json:"id"`
	Node string `json:"node"`
	Mode string `json:"mode"`
	In   bool   `json:"in"` // the primary waits for it now
}

// SyncView is an instance's synchronous replication: the choice, and plainly
// whether writes are protected right now.
type SyncView struct {
	Prefer     bool          `json:"prefer"`
	Required   bool          `json:"required"`
	LimitMS    int           `json:"limit_ms"`
	Protection string        `json:"protection"` // off, protected, unprotected or blocked
	Status     string        `json:"status"`     // the same in words
	Standbys   []SyncStandby `json:"standbys"`
}

// SyncOf is tile tileID's synchronous replication.
func (o *Orchestrator) SyncOf(ctx context.Context, tileID string) (SyncView, error) {
	_, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return SyncView{}, err
	}
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil {
		return SyncView{}, err
	}
	v := SyncView{Prefer: inst.PreferSync, Required: inst.SyncRequired, LimitMS: inst.SyncLimitMS, Protection: SyncOff, Standbys: []SyncStandby{}}
	for _, m := range ms {
		if m.Role == opmember.Replica && m.State != opmember.Removing {
			v.Standbys = append(v.Standbys, SyncStandby{ID: m.ID, Node: o.nodeName(ctx, m.NodeID), Mode: cmpMode(m.SyncMode), In: m.SyncIn})
		}
	}
	if inst.PreferSync {
		v.Protection = SyncUnprotected // until the first pass says
		if c := o.syncCtlOf(inst.ID); c.seen {
			v.Protection = c.last.Protection
		}
	}
	v.Status = syncWords(v.Protection, len(v.Standbys))
	if inst.PreferSync && v.Protection == SyncUnprotected && !o.syncCtlOf(inst.ID).seen {
		v.Status = "Checking."
	}
	return v, nil
}

// SetSyncIn is what a change of the instance's choice may carry; nil leaves a
// field as it is.
type SetSyncIn struct {
	Prefer   *bool `json:"prefer"`
	Required *bool `json:"required"`
	LimitMS  *int  `json:"limit_ms"`
}

// SetSync stores the choice and applies it at once.
func (o *Orchestrator) SetSync(ctx context.Context, tileID string, in SetSyncIn) (SyncView, error) {
	t, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return SyncView{}, err
	}
	if inst.Engine != "postgres" {
		return SyncView{}, errs.Invalidf("engine", "Only postgres has synchronous replication.")
	}
	prefer, required, limit := inst.PreferSync, inst.SyncRequired, inst.SyncLimitMS
	if in.Prefer != nil {
		prefer = *in.Prefer
	}
	if in.Required != nil {
		required = *in.Required
	}
	if in.LimitMS != nil {
		limit = *in.LimitMS
	}
	if limit <= 0 {
		limit = DefaultSyncLimitMS
	}
	if inst, err = o.operated.SetSync(ctx, inst, prefer, limit, required); err != nil {
		return SyncView{}, err
	}
	if err := o.syncInstance(ctx, t, inst); err != nil && err != errSyncBusy {
		slog.Warn("sync: not applied yet, the loop retries", "instance", inst.ID, "err", err)
	}
	return o.SyncOf(ctx, tileID)
}

// SetStandbySync sets a standby's mode (auto, always or never).
func (o *Orchestrator) SetStandbySync(ctx context.Context, tileID, memberID, mode string) (SyncView, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !SyncModeOK(mode) {
		return SyncView{}, errs.Invalidf("mode", "The mode is auto, always or never.")
	}
	m, err := o.standbyOf(ctx, tileID, memberID)
	if err != nil {
		return SyncView{}, err
	}
	if _, err := o.members.SetSync(ctx, m, mode, m.SyncIn); err != nil {
		return SyncView{}, err
	}
	t, inst, err := o.memberInstance(ctx, tileID)
	if err == nil {
		if err := o.syncInstance(ctx, t, inst); err != nil && err != errSyncBusy {
			slog.Warn("sync: not applied yet, the loop retries", "instance", inst.ID, "err", err)
		}
	}
	return o.SyncOf(ctx, tileID)
}

// syncFirst orders members with the ones the primary waited for first, for a
// promote to prefer: a commit the primary confirmed is on one of them.
func syncFirst(ms []InstanceMember) (waited, rest []InstanceMember) {
	for _, m := range ms {
		if m.SyncIn {
			waited = append(waited, m)
		} else {
			rest = append(rest, m)
		}
	}
	return waited, rest
}
