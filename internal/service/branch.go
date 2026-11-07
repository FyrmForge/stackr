package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// BranchIn is what BranchSlice takes; every field is optional.
type BranchIn struct {
	Env      string `json:"env"`      // the env the branch goes to: <env> of the source's stack, or <stack>:<env>; default the source's
	Instance string `json:"instance"` // the postgres tile it lives on: <stack>:<env>:<tile>, or a tile of Env; default the source's
	Expires  string `json:"expires"`  // 3d, 36h ...; default never
}

const branchAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var branchSuffix = regexp.MustCompile(`-br-[a-z0-9]{4}$`)

// ParseExpiry is a Go duration, or whole days with a d suffix (3d).
func ParseExpiry(s string) (time.Duration, error) {
	var d time.Duration
	var err error
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if days, err = strconv.Atoi(n); err == nil {
			d = time.Duration(days) * 24 * time.Hour
		}
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil || d <= 0 {
		return 0, errs.Invalidf("expires", "%q is not a duration; use 3d or 36h", s)
	}
	return d, nil
}

// BranchSlice cuts a copy of postgres slice sliceID, as of now, into a new
// slice tile <slice>-br-<4 random> with its own owner role and password, and
// queues the job that provisions it and loads the data: pg_dump of the source
// piped straight into the load, no file. No bindings are copied. The caller
// needs tile.write on the source's stack and on the target's.
// ponytail: the dump runs against the live instance; fine at stackr's sizes.
func (o *Orchestrator) BranchSlice(ctx context.Context, by *Principal, sliceID string, in BranchIn) (Tile, Job, error) {
	src, err := o.sliceTile(ctx, sliceID)
	if err != nil {
		return Tile{}, Job{}, err
	}
	srcInst, err := o.postgresOf(ctx, src)
	if err != nil {
		return Tile{}, Job{}, err
	}
	var expires *time.Time
	if in.Expires != "" {
		d, err := ParseExpiry(in.Expires)
		if err != nil {
			return Tile{}, Job{}, err
		}
		t := time.Now().UTC().Add(d)
		expires = &t
	}
	srcEnv, err := o.envs.Get(ctx, src.EnvironmentID)
	if err != nil {
		return Tile{}, Job{}, err
	}
	srcStack, err := o.stacks.Get(ctx, src.StackID)
	if err != nil {
		return Tile{}, Job{}, err
	}
	if err := mayWrite(by, srcStack); err != nil {
		return Tile{}, Job{}, err
	}
	st, env, err := o.branchEnv(ctx, srcStack, srcEnv, in.Env)
	if err != nil {
		return Tile{}, Job{}, err
	}
	if err := mayWrite(by, st); err != nil {
		return Tile{}, Job{}, err
	}
	if st.ConfigRepo != "" {
		return Tile{}, Job{}, errs.Conflictf("stack %s is config-managed; a branch there would be swept by its stack file", st.Slug)
	}
	from, err := o.branchFrom(ctx, srcInst, st, env, in.Instance)
	if err != nil {
		return Tile{}, Job{}, err
	}
	drop := tile.Drop
	row := Tile{
		StackID:       st.ID,
		EnvironmentID: env.ID,
		Kind:          tile.Slice,
		ProvisionFrom: &from,
		OnRemove:      &drop,
		ExpiresAt:     expires,
		BranchOf:      &src.ID,
	}
	base := branchSuffix.ReplaceAllString(src.Slug, "")
	for { // a taken suffix draws again
		row.Name = base + "-br-" + randomSuffix(4)
		row.Slug = row.Name
		if _, err = o.tiles.GetBySlug(ctx, env.ID, row.Slug); errors.Is(err, errs.ErrNotFound) {
			break
		}
		if err != nil {
			return Tile{}, Job{}, err
		}
	}
	// after the slug: the allow list can name the consumer tile
	inst, err := o.deploy.Target(ctx, row)
	if err != nil {
		return Tile{}, Job{}, err
	}
	if m, err := o.managed.GetByTile(ctx, inst.ID); err != nil || m.Engine != "postgres" {
		return Tile{}, Job{}, errs.Invalidf("instance", "%s is not a postgres instance", inst.Slug)
	}
	if err := o.gateTile(ctx, inst.ID, true); err != nil {
		return Tile{}, Job{}, err
	}
	br, err := o.tiles.Create(ctx, row)
	if err != nil {
		return Tile{}, Job{}, err
	}
	lock := []string{inst.ID, br.ID}
	if srcInst.ID != inst.ID {
		lock = append(lock, srcInst.ID)
	}
	j, err := o.enqueue(ctx, kindSeed, seedJob{
		TileID:  inst.ID,
		SliceID: br.ID,
		Format:  managed.DumpCustom,
		Source:  seedSource{Kind: "slice", SliceID: src.ID},
	}, lock...)
	if err != nil {
		_ = o.store.Tiles.Delete(ctx, br.ID)
		return Tile{}, Job{}, err
	}
	o.auditBy(ctx, by, securitylog.SliceBranch, br.ID, fmt.Sprintf("%s:%s:%s -> %s:%s:%s on %s, at=now",
		srcStack.Slug, srcEnv.Slug, src.Slug, st.Slug, env.Slug, br.Slug, from))
	return br, j, nil
}

// postgresOf is the postgres instance tile slice s is cut on.
func (o *Orchestrator) postgresOf(ctx context.Context, s Tile) (Tile, error) {
	p, ok, err := o.managed.ProvisionOf(ctx, s.ID)
	if err != nil {
		return Tile{}, err
	}
	if !ok {
		return Tile{}, errs.Conflictf("slice %s is not deployed yet; deploy it first", s.Slug)
	}
	m, err := o.managed.Get(ctx, p.InstanceID)
	if err != nil {
		return Tile{}, err
	}
	if m.Engine != "postgres" {
		return Tile{}, errs.Invalidf("slice", "slice %s is %s; only postgres slices branch", s.Slug, m.Engine)
	}
	return o.tiles.Get(ctx, m.TileID)
}

// branchEnv is the env a branch goes to: ref "" = the source's own, else an
// env of the source's stack, or <stack>:<env> in the same org.
func (o *Orchestrator) branchEnv(ctx context.Context, srcStack Stack, srcEnv Environment, ref string) (Stack, Environment, error) {
	if ref == "" {
		return srcStack, srcEnv, nil
	}
	st := srcStack
	stackSlug, envSlug, two := strings.Cut(ref, ":")
	if two {
		var err error
		if st, err = o.stacks.GetBySlug(ctx, srcStack.OrgID, stackSlug); err != nil {
			return st, Environment{}, notFoundAs(err, errs.Invalidf("env", "no stack %s in this org", stackSlug))
		}
	} else {
		envSlug = ref
	}
	env, err := o.envs.GetBySlug(ctx, st.ID, envSlug)
	if err != nil {
		return st, env, notFoundAs(err, errs.Invalidf("env", "no env %s in stack %s", envSlug, st.Slug))
	}
	return st, env, nil
}

func notFoundAs(err, as error) error {
	if errors.Is(err, errs.ErrNotFound) {
		return as
	}
	return err
}

// branchFrom is the branch's provision_from: the source's own instance, or
// the one named (<stack>:<env>:<tile>, or a bare tile of the target env).
func (o *Orchestrator) branchFrom(ctx context.Context, srcInst Tile, st Stack, env Environment, instance string) (string, error) {
	switch {
	case instance == "":
		e, err := o.envs.Get(ctx, srcInst.EnvironmentID)
		if err != nil {
			return "", err
		}
		s, err := o.stacks.Get(ctx, srcInst.StackID)
		if err != nil {
			return "", err
		}
		return s.Slug + ":" + e.Slug + ":" + srcInst.Slug, nil
	case strings.Count(instance, ":") == 2:
		return instance, nil
	case !strings.Contains(instance, ":"):
		return st.Slug + ":" + env.Slug + ":" + instance, nil
	}
	return "", errs.Invalidf("instance", "%q is not <stack>:<env>:<tile> or a tile of the target env", instance)
}

// mayWrite is tile.write on stack st for by.
func mayWrite(by *Principal, st Stack) error {
	if by == nil {
		return errs.Refusedf("branching needs a signed-in user")
	}
	return authz.Can(by.Access, "tile.write", authz.Resource{OrgID: st.OrgID, StackID: st.ID})
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = branchAlphabet[int(b[i])%len(branchAlphabet)]
	}
	return string(b)
}

// auditBy writes a security-log row for by; a failed write is logged, the
// act stands.
func (o *Orchestrator) auditBy(ctx context.Context, by *Principal, action, tileID, detail string) {
	user, key := actorOf(by)
	err := securitylog.New(o.store.SecurityEvents).Append(ctx, securitylog.Event{
		Action: action, ActorID: user, KeyID: key, TargetKind: "tile", TargetID: tileID, Detail: detail,
	})
	if err != nil {
		slog.Error("audit", "action", action, "error", err.Error())
	}
}

// sweepExpired deletes the slice tiles whose expires_at has passed, through
// the normal tile delete (their on_remove drop takes the database).
func (o *Orchestrator) sweepExpired(ctx context.Context) {
	ss, err := o.tiles.ListByKind(ctx, tile.Slice)
	if err != nil {
		slog.Warn("branches: expiry sweep skipped", "err", err)
		return
	}
	now := time.Now().UTC()
	for _, s := range ss {
		if s.ExpiresAt == nil || s.ExpiresAt.After(now) {
			continue
		}
		if _, err := o.DeleteTile(ctx, s.ID); err != nil {
			slog.Warn("branches: expired slice not deleted", "tile", s.ID, "err", err)
			continue
		}
		slog.Info("branches: expired slice queued for deletion", "tile", s.ID, "slug", s.Slug)
	}
}

// SliceCreds are a slice's own owner credentials on its instance's port.
type SliceCreds struct {
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	Port     int    `json:"port"`
}

// SliceCreds reveals the owner credentials of postgres slice sliceID to a
// writer of its stack, and logs the reveal; no other cred is read.
func (o *Orchestrator) SliceCreds(ctx context.Context, by *Principal, sliceID string) (SliceCreds, error) {
	s, err := o.sliceTile(ctx, sliceID)
	if err != nil {
		return SliceCreds{}, err
	}
	st, err := o.stacks.Get(ctx, s.StackID)
	if err != nil {
		return SliceCreds{}, err
	}
	if err := mayWrite(by, st); err != nil {
		return SliceCreds{}, err
	}
	if _, err := o.postgresOf(ctx, s); err != nil {
		return SliceCreds{}, err
	}
	p, _, err := o.managed.ProvisionOf(ctx, s.ID)
	if err != nil {
		return SliceCreds{}, err
	}
	o.auditBy(ctx, by, securitylog.SecretReveal, s.ID, "slice owner credentials of "+p.DBName)
	return SliceCreds{
		Database: p.DBName,
		User:     p.DBUser,
		Password: p.DBPassword,
		Port:     managed.Engines["postgres"].Definition().Port,
	}, nil
}

// DialSlice is DialTile to the instance postgres slice sliceID lives on: a
// slice tile has no container of its own.
func (o *Orchestrator) DialSlice(ctx context.Context, sliceID string) (net.Conn, int, error) {
	s, err := o.sliceTile(ctx, sliceID)
	if err != nil {
		return nil, 0, err
	}
	inst, err := o.postgresOf(ctx, s)
	if err != nil {
		return nil, 0, err
	}
	return o.DialTile(ctx, inst.ID, "", 0)
}
