package service

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
)

// builderCand is a node a build could run on.
type builderCand struct {
	ID   string
	Free uint64 // available memory from the node's facts, 0 = unknown
}

// pickBuilder is the candidate with the most available memory. The
// controller wins ties and is the answer when there is no candidate: a build
// always has somewhere to run.
func pickBuilder(ctl string, cands []builderCand) string {
	best, free := ctl, uint64(0)
	for _, c := range cands {
		if c.ID == ctl {
			free = max(free, c.Free)
		}
	}
	for _, c := range cands {
		if c.Free > free {
			best, free = c.ID, c.Free
		}
	}
	return best
}

// builderNode picks where a build runs: the builder-tagged nodes that are
// online (the controller always counts as up), by available memory.
func (o *Orchestrator) builderNode(ctx context.Context, ctl string) string {
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return ctl
	}
	var cands []builderCand
	for _, n := range ns {
		if !slices.Contains(node.Tags(n), node.TagBuilder) || n.ID != ctl && n.Status != node.StatusOnline {
			continue
		}
		cands = append(cands, builderCand{ID: n.ID, Free: o.memFree(n.ID)})
	}
	return pickBuilder(ctl, cands)
}

// memFree is a node's available memory from its last known facts.
func (o *Orchestrator) memFree(id string) uint64 {
	if o.mesh == nil {
		return 0
	}
	o.mesh.mu.Lock()
	defer o.mesh.mu.Unlock()
	return o.mesh.facts[id].MemFree
}

// buildFallback builds on at. A worker whose build failed and that no longer
// answers dropped out mid-build, so the build runs again on the controller;
// one that answers failed the build itself, and that error stands.
func buildFallback(at, ctl string, build func(node string) (string, error), answers func(node string) bool, log io.Writer) (string, error) {
	id, err := build(at)
	if err == nil || at == ctl || answers(at) {
		return id, err
	}
	_, _ = fmt.Fprintln(log, "The builder node dropped out; building on the controller.")
	return build(ctl)
}

// nodeAnswers reports a worker that answers its extras API: a build that
// failed there is the build's fault, not a node that dropped out.
func (o *Orchestrator) nodeAnswers(ctx context.Context, id string) bool {
	c, err := o.nodeExtras(id)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = c.Facts(ctx)
	return err == nil
}

// buildRepos are the repos each builder node may push while it builds them:
// the registry's node facts read them (PushRepos), so a builder can push a
// build and nothing else.
type buildRepos struct {
	mu sync.Mutex
	m  map[string][]string // node id -> repos
}

// hold lets node push repo until the returned release runs.
func (b *buildRepos) hold(nodeID, repo string) (release func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string][]string{}
	}
	b.m[nodeID] = append(b.m[nodeID], repo)
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		rs := b.m[nodeID]
		if i := slices.Index(rs, repo); i >= 0 {
			b.m[nodeID] = slices.Delete(rs, i, i+1)
		}
	}
}

func (b *buildRepos) of(nodeID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.m[nodeID])
}
