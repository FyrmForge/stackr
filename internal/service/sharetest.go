package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// ShareTest is the answer of TestShare. Raw is the Docker text behind a
// failure, secrets stripped.
type ShareTest struct {
	OK      bool   `json:"ok"`
	Node    string `json:"node"`
	Message string `json:"message"`
	Raw     string `json:"raw"`
}

const (
	shareTestTotal = 90 * time.Second // pull, mount and run together
	shareTestRun   = 20 * time.Second // the helper's own run
	shareTestClean = 15 * time.Second // removal, after the rest has timed out
)

// TestShare mounts the share once, read-only, on the node a deploy would use
// and says whether it can be read. The host mounts a LAN export, so it runs
// only for a server admin or when a stack of the org holds an admin grant
// for this share at its pinned address. Nothing is saved; the test volume
// is removed again.
func (o *Orchestrator) TestShare(ctx context.Context, by *Principal, orgID, ref string) (ShareTest, error) {
	ctx, cancel := context.WithTimeout(ctx, shareTestTotal)
	defer cancel()
	s, err := o.volumes.ShareOf(ctx, orgID, ref)
	if err != nil {
		return ShareTest{}, err
	}
	addr, err := o.deploy.ShareAddr(ctx, s)
	if err != nil {
		return ShareTest{}, err
	}
	// A non-admin may use only the stacks that hold the grant: the node and
	// env come from a tile in one of them.
	var approved map[string]bool
	nonAdmin := by == nil || authz.Can(by.Access, "hostgrant.approve", authz.Resource{}) != nil
	if nonAdmin {
		if approved, err = o.approvedStacks(ctx, orgID, deploy.ShareGrant(s.Slug, addr)); err != nil {
			return ShareTest{}, err
		}
		if len(approved) == 0 {
			return ShareTest{}, errs.Refusedf("An admin has to approve this share on a stack first.")
		}
	}
	// The node and the env a mounting tile reads its params in.
	var env store.Environment
	var stack store.Stack
	found := false
	if err := o.eachShareLine(ctx, orgID, func(t store.Tile, m tile.Mount) {
		if found || m.Share != s.Slug || t.EnvironmentID == "" || (nonAdmin && !approved[t.StackID]) {
			return
		}
		e, err := o.envs.Get(ctx, t.EnvironmentID)
		if err != nil {
			return
		}
		st, err := o.stacks.Get(ctx, t.StackID)
		if err != nil {
			return
		}
		env, stack, found = e, st, true
	}); err != nil {
		return ShareTest{}, err
	}
	if nonAdmin && !found {
		return ShareTest{}, errs.Conflictf("Mount share %s on a tile in an approved stack first.", s.Slug)
	}
	var nctx context.Context
	if found {
		nctx, err = o.withEnvNode(ctx, env)
	} else {
		nctx, err = o.withController(ctx)
	}
	if err != nil {
		return ShareTest{}, err
	}
	nodeName := ""
	if id, ok := docker.NodeFrom(nctx); ok {
		if n, err := o.nodeLeaf().Get(ctx, id); err == nil {
			nodeName = n.Name
		}
	}

	var user, pass string
	if s.User != "" || s.PasswordRef != "" {
		if !found {
			return ShareTest{}, errs.Conflictf("Mount share %s on a tile first: its login is read in an environment.", s.Slug)
		}
		snap, err := o.deploy.ParamSnapshot(ctx, env, stack, true)
		if err != nil {
			return ShareTest{}, err
		}
		rr := params.NewResolver(snap)
		if user, err = rr.Expand(params.InEnv, s.User); err != nil {
			return ShareTest{}, err
		}
		if pass, err = rr.Expand(params.InEnv, s.PasswordRef); err != nil {
			return ShareTest{}, err
		}
	}
	clean := func(raw string) string {
		return volume.MaskMount(raw, pass)
	}
	fail := func(raw string) ShareTest {
		return ShareTest{Node: nodeName, Message: "Could not mount share " + s.Slug + " from " + nodeName + ".", Raw: clean(raw)}
	}
	denied := func(raw string) ShareTest {
		return ShareTest{Node: nodeName, Message: volume.ShareDenied(s), Raw: clean(raw)}
	}

	name, err := o.volumes.EnsureShareProbe(nctx, s, addr, user, pass)
	if err != nil {
		return fail(err.Error()), nil
	}
	// A volume of its own, so the deploy's is never made or removed here:
	// removed when the test is done, after the helper is gone (defers run last
	// in, first out).
	defer func() {
		c, stop := context.WithTimeout(context.WithoutCancel(nctx), shareTestClean)
		defer stop()
		_ = o.docker.RemoveVolume(c, name)
	}()
	if err := o.docker.EnsureTool(nctx); err != nil {
		return fail(err.Error()), nil
	}
	id, err := o.docker.Run(nctx, docker.ContainerSpec{
		Name:       "stackr-share-test-" + uuid.NewString()[:8],
		Image:      docker.ToolImage,
		Entrypoint: []string{"ls"},
		Cmd:        []string{"/data"},
		Volumes:    []string{name + ":/data:ro,nocopy"},
		Labels:     map[string]string{docker.LabelManaged: "true"},
		Restart:    "no",
	})
	if id != "" {
		// Removed on every path, even after the caller's ctx has ended.
		defer func() {
			c, stop := context.WithTimeout(context.WithoutCancel(nctx), shareTestClean)
			defer stop()
			_ = o.docker.StopRemove(c, id)
		}()
	}
	if err != nil {
		var md docker.MountDeniedError
		if errors.As(err, &md) {
			return denied(md.Raw), nil
		}
		return fail(err.Error()), nil
	}
	wctx, stop := context.WithTimeout(nctx, shareTestRun)
	defer stop()
	code, err := o.docker.Wait(wctx, id)
	if err != nil {
		return fail(err.Error()), nil
	}
	if code == 0 {
		return ShareTest{OK: true, Node: nodeName, Message: "Readable from " + nodeName + "."}, nil
	}
	lctx, stopLogs := context.WithTimeout(context.WithoutCancel(nctx), shareTestClean)
	defer stopLogs()
	out, _ := o.docker.Logs(lctx, id, 20)
	if strings.Contains(out, "ermission denied") {
		return denied(out), nil
	}
	return fail(out), nil
}

// approvedStacks are the stacks of the org whose grant holds perm.
func (o *Orchestrator) approvedStacks(ctx context.Context, orgID, perm string) (map[string]bool, error) {
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, st := range sts {
		have, err := o.hostgrant.Of(ctx, st.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range have.Lines {
			if _, p := hostgrant.Split(l); p == perm {
				out[st.ID] = true
			}
		}
	}
	return out, nil
}
