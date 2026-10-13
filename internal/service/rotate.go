package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Mesh key rotation: a node's WireGuard and exit keys are replaced without a
// rejoin. A worker makes its new keys itself and hands the public halves over
// through the recovery path (its cert proves who it is, so this works while
// its tunnels are down); RecoverNode takes them. The controller stages its
// own, pushes the workers its new public key, farthest first, then switches.
// One node at a time, across the cluster, on a button or a schedule.

const (
	// RotateEveryKey is the schedule in days ("" or "0" = off).
	RotateEveryKey = "mesh_key_rotate_days"
	rotatedAtKey   = "mesh_key_rotated_at"
)

var (
	// rotateWait is how long a worker may take to hand its new keys over:
	// it reverts after worker.RotateRevertAfter.
	rotateWait = 3 * time.Minute
	// rotateSettle is the gate before the next node: no pair may have gone
	// down this recently, and the last node's new key is heard (or a minute
	// passed).
	rotateSettle = 2 * time.Minute
	rotateHeard  = time.Minute
	rotatePoll   = 2 * time.Second
)

// rotation is the controller's state: one node at a time; the trigger of a
// worker rotation waits here until its new keys come in.
type rotation struct {
	mu      sync.Mutex
	trigger sync.Map // node id -> "manual by <user>" | "schedule"
}

// RotateResult is one node's outcome.
type RotateResult struct {
	Node  string `json:"node"`
	Error string `json:"error,omitempty"`
}

// RotateNodeKeys starts a rotation of one node's keys ("" = every node, one
// at a time) and returns at once; the security log has the outcome.
func (o *Orchestrator) RotateNodeKeys(ctx context.Context, by *Principal, ref string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	if o.mesh == nil {
		return errs.Conflictf("this install runs no mesh")
	}
	trigger := "manual by " + by.User.Email
	if ref == "" {
		go o.rotateAll(o.rotateCtx(), trigger)
		return nil
	}
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return err
	}
	if n.Status == node.StatusPending {
		return errs.Conflictf("%s has not joined yet", n.Name)
	}
	go func() {
		o.rotation.mu.Lock()
		defer o.rotation.mu.Unlock()
		if err := o.rotateOne(o.rotateCtx(), n, trigger); err != nil {
			slog.Warn("mesh: rotate keys", "node", n.Name, "err", err)
		}
	}()
	return nil
}

func (o *Orchestrator) rotateCtx() context.Context {
	if o.meshCtx != nil {
		return o.meshCtx
	}
	return context.Background()
}

// rotateAll rotates every joined node, workers first and the controller
// last, waiting for the mesh to settle between nodes.
func (o *Orchestrator) rotateAll(ctx context.Context, trigger string) []RotateResult {
	o.rotation.mu.Lock()
	defer o.rotation.mu.Unlock()
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return []RotateResult{{Error: err.Error()}}
	}
	slices.SortStableFunc(ns, func(a, b store.Node) int {
		return boolCmp(a.Role == node.Controller, b.Role == node.Controller)
	})
	var out []RotateResult
	for _, n := range ns {
		if n.Status == node.StatusPending || (n.Role == node.Worker && n.Status != node.StatusOnline) {
			continue // a node that is not up cannot rotate; it keeps its keys
		}
		o.waitSettled(ctx)
		r := RotateResult{Node: n.Name}
		if err := o.rotateOne(ctx, n, trigger); err != nil {
			r.Error = err.Error()
		}
		out = append(out, r)
		if ctx.Err() != nil {
			break
		}
	}
	_ = o.store.Settings.Set(ctx, rotatedAtKey, strconv.FormatInt(time.Now().Unix(), 10))
	return out
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// waitSettled holds the next rotation until no pair went down within
// rotateSettle and the last rotated key is heard, or rotateSettle passed.
func (o *Orchestrator) waitSettled(ctx context.Context) {
	end := time.Now().Add(rotateSettle)
	for time.Now().Before(end) && !o.mesh.settled(time.Now()) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(rotatePoll):
		}
	}
}

// rotateOne rotates one node; o.rotation.mu is held.
func (o *Orchestrator) rotateOne(ctx context.Context, n store.Node, trigger string) error {
	if n.Role == node.Controller {
		return o.rotateController(ctx, n, trigger)
	}
	cl, ok := o.mesh.nodes(n.ID)
	if !ok {
		return fmt.Errorf("%s: no connection", n.Name)
	}
	r, ok := cl.(interface{ RotateKeys(context.Context) error })
	if !ok {
		return fmt.Errorf("%s: cannot rotate", n.Name)
	}
	old := n.WGPublicKey
	o.rotation.trigger.Store(n.ID, trigger)
	if err := r.RotateKeys(ctx); err != nil {
		o.rotation.trigger.Delete(n.ID)
		return err
	}
	// The worker swaps its keys and recovers through the panel: RecoverNode
	// stores the new key.
	end := time.Now().Add(rotateWait)
	for time.Now().Before(end) {
		if cur, err := o.store.Nodes.Get(ctx, n.ID); err == nil && cur.WGPublicKey != old {
			o.mesh.awaitHeard(cur.WGPublicKey)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rotatePoll):
		}
	}
	o.rotation.trigger.Delete(n.ID)
	return fmt.Errorf("%s did not hand its new keys over; it goes back to the old ones", n.Name)
}

// rotateController stages the controller's new keys, pushes every worker
// the new public key (farthest first, so a routed node hears it while its
// hop still talks the old key), then switches. A worker that misses the
// push recovers through the panel.
func (o *Orchestrator) rotateController(ctx context.Context, n store.Node, trigger string) error {
	if o.localWorker == nil {
		return errors.New("the controller runs no mesh")
	}
	wg, exit, err := o.localWorker.StageKeys()
	if err != nil {
		return err
	}
	m := o.mesh
	m.syncMu.Lock() // no sync may push the old key between these pushes
	defer m.syncMu.Unlock()
	m.mu.Lock()
	old := m.wgPK[n.ID]
	m.mu.Unlock()
	m.tookKeys(n.ID, old, wg, exit)
	in, specs, err := m.plan(ctx)
	if err != nil {
		return err
	}
	var workers []string
	for id := range specs {
		if id != n.ID {
			workers = append(workers, id)
		}
	}
	slices.SortStableFunc(workers, func(a, b string) int { return hops(in.Next, n.ID, b) - hops(in.Next, n.ID, a) })
	for _, id := range workers {
		// A direct worker applies the new key mid-request, so its reply
		// crosses the tunnel it just rekeyed and never arrives: do not wait
		// the client's full timeout for it. The spec is applied either way;
		// a push that really failed is retried by recovery.
		pctx, cancel := context.WithTimeout(ctx, rotatePushWait)
		_ = m.push(pctx, in, specs, []string{id}, true)
		cancel()
	}
	if err := o.localWorker.PromoteKeys(); err != nil {
		return err
	}
	_ = m.push(ctx, in, specs, []string{n.ID}, true)
	o.auditRotate(ctx, n, old, wg, trigger)
	m.Kick()
	return nil
}

// rotatePushWait is how long a controller rotation waits for each worker's
// reply to the push that carries its new key.
var rotatePushWait = 5 * time.Second // a var for tests

// hops is how many hops src takes to reach dst on next (1 = direct).
func hops(next map[[2]string]string, src, dst string) int {
	n := 1
	for cur := src; n < 64; n++ {
		via := next[[2]string{cur, dst}]
		if via == "" {
			return n
		}
		cur = via
	}
	return n
}

// takeNodeKeys stores the public keys a worker reported through the panel
// when they differ from what the controller has: a rotation it asked for, or
// one the node finished after a restart.
func (o *Orchestrator) takeNodeKeys(ctx context.Context, n store.Node, wg, exit string) error {
	for _, k := range []string{wg, exit} {
		if _, err := wgtypes.ParseKey(k); err != nil {
			return errs.Refusedf("not a WireGuard key")
		}
	}
	old := n.WGPublicKey
	n.WGPublicKey = wg
	if err := o.store.Nodes.Update(ctx, n); err != nil {
		return err
	}
	o.mesh.tookKeys(n.ID, old, wg, exit)
	trigger := "by the node"
	if v, ok := o.rotation.trigger.LoadAndDelete(n.ID); ok {
		trigger = v.(string)
	}
	o.auditRotate(ctx, n, old, wg, trigger)
	return nil
}

func (o *Orchestrator) auditRotate(ctx context.Context, n store.Node, old, wg, trigger string) {
	_ = o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.NodeKeyRotate, TargetKind: "node", TargetID: n.ID,
			Detail: fmt.Sprintf("%s: %s -> %s, %s", n.Name, keyPrint(old), keyPrint(wg), trigger),
		})
	})
	slog.Info("mesh: node keys rotated", "node", n.Name, "trigger", trigger)
}

// keyPrint is a short, public fingerprint of a public key.
func keyPrint(k string) string {
	if len(k) < 8 {
		return "none"
	}
	return k[:8]
}

// rotateSchedule runs a cluster rotation when the schedule says it is due.
func (o *Orchestrator) rotateSchedule(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if o.rotateDue(ctx, time.Now()) {
				o.rotateAll(ctx, "schedule")
			}
		}
	}
}

func (o *Orchestrator) rotateDue(ctx context.Context, now time.Time) bool {
	days, _ := o.settingInt(ctx, RotateEveryKey)
	if days <= 0 {
		return false
	}
	last, _ := o.settingInt(ctx, rotatedAtKey)
	if last == 0 {
		// First run of a fresh schedule: count from now, not 1970.
		_ = o.store.Settings.Set(ctx, rotatedAtKey, strconv.FormatInt(now.Unix(), 10))
		return false
	}
	return now.Sub(time.Unix(int64(last), 0)) >= time.Duration(days)*24*time.Hour
}

func (o *Orchestrator) settingInt(ctx context.Context, k string) (int, error) {
	v, _, err := o.store.Settings.Get(ctx, k)
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.Atoi(v)
}

// RotateSchedule is the schedule in days, 0 = off.
func (o *Orchestrator) RotateSchedule(ctx context.Context) int {
	d, _ := o.settingInt(ctx, RotateEveryKey)
	return d
}

// SetRotateSchedule sets the schedule in days, 0 = off.
func (o *Orchestrator) SetRotateSchedule(ctx context.Context, by *Principal, days int) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	if days < 0 || days > 365 {
		return errs.Invalidf("days", "between 0 (off) and 365")
	}
	if days > 0 {
		_ = o.store.Settings.Set(ctx, rotatedAtKey, strconv.FormatInt(time.Now().Unix(), 10))
	}
	return o.store.Settings.Set(ctx, RotateEveryKey, strconv.Itoa(days))
}

// tookKeys records a node's new public keys: traffic samples and pushes made
// with the old key go.
func (m *meshSvc) tookKeys(id, oldWG, wg, exit string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rx, id)
	for _, peers := range m.rx {
		delete(peers, oldWG)
	}
	m.wgPK[id], m.exitPK[id] = wg, exit
	m.pushed = map[string][32]byte{}
	m.heard = wg
	m.heardBy = time.Now().Add(rotateHeard)
}

// awaitHeard notes the key the settle gate waits to hear.
func (m *meshSvc) awaitHeard(wg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heard, m.heardBy = wg, time.Now().Add(rotateHeard)
}

// settled: no pair started looking down within rotateSettle, and the last
// rotated key carried traffic (or rotateHeard passed).
func (m *meshSvc) settled(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, since := range m.down {
		if now.Sub(since) < rotateSettle {
			return false
		}
	}
	if m.heard == "" || now.After(m.heardBy) {
		return true
	}
	for _, peers := range m.rx {
		if s, ok := peers[m.heard]; ok && !s.changed.IsZero() {
			return true
		}
	}
	return false
}
