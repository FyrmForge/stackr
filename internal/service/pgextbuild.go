package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Postgres recipes on operated instances (Stage 2): the plan of an enable, the
// recipe list, the apply bound to a reviewed plan (R21), the image build and
// the rebuild a deploy asks for when the image is gone.

// pgextplanKey is an instance's last recipe plan: pgextplan.<instance id>,
// JSON of PGExtPlan.
const pgextplanKey = "pgextplan."

// pgBuildWalkMax caps how many builds a failed boot check walks down per
// build: each step moves every package of the failing recipe one build older.
const pgBuildWalkMax = 3

// PGPkgChange is one package's version change in a plan: From "" is a new
// package, To "" a removed one.
type PGPkgChange struct {
	Name, From, To string
}

// PGExtPlanIn is what a plan decided and the apply builds from: the instance
// list it sets, the recipe names that list asks for, the resolved package pins,
// the base image, the CPU type and node, the image ref the build gets and the
// startup modules the recipes need.
type PGExtPlanIn struct {
	Extensions []string
	Names      []string
	Pins       map[string]string
	Base       string
	Arch       string
	NodeID     string
	Image      string
	Preload    []string
	// Update: the plan moves the base to the newest minor (UpdatePGVersion).
	Update bool
}

// PGExtPlan is the reviewed plan of a recipe change (R21): its id and a hash
// over its inputs and the instance's base state (flow/operated.PlanHash);
// apply must name the id and is refused when the hash no longer matches
// (message 28). Lines are the rendered plan; Build says an image is built,
// Restart that the instance restarts; Versions are the package changes against
// ext_build.packages; PGUpdate is the Postgres update line ("" = none); Slices
// are the slices on the instance; Blockers refuse the apply. In is what apply
// builds from.
type PGExtPlan struct {
	ID, Hash string
	Lines    []string
	Build    bool
	Restart  bool
	Versions []PGPkgChange
	PGUpdate string
	Slices   []string
	Blockers []string
	In       PGExtPlanIn
}

// approvalPlan is the plan as the house approve contract reads it: a blocker
// per refusal, and an impact line when the instance restarts (so a missing
// Confirm is refused, V6).
func (p PGExtPlan) approvalPlan() planfile.Plan {
	pl := planfile.Plan{Blockers: p.Blockers}
	imp := ""
	if p.Restart {
		imp = p.restartLine()
	}
	pl.Add(planfile.Change{Kind: "extensions", Tile: p.ID, Impact: imp})
	return pl
}

func (p PGExtPlan) restartLine() string {
	for _, l := range p.Lines {
		if strings.Contains(l, "restarts. A startup module") {
			return l
		}
	}
	return "The instance restarts."
}

// PGRecipeView is a recipe as the admin page, API and CLI show it: a built-in
// or an admin-added one, switched on or off, and who may use it (Share is
// ["all"] or org ids).
type PGRecipeView struct {
	Name       string
	Builtin    bool
	Enabled    bool
	Majors     []int
	Packages   []string
	PackageURL string
	SHA256     map[string]string
	Version    string
	Extensions []string
	Preload    []string
	Requires   []string
	Share      []string
}

// pgRecipeFns are the recipe engine's functions (lane A2's). The service
// reaches them through this table so a test can give an Orchestrator its own.
type pgRecipeFns struct {
	Check      func(mflow.Recipe) error
	Resolve    func(names []string, major int, orgID string, rows []store.PGRecipe) ([]mflow.Recipe, error)
	Preload    func([]mflow.Recipe) []string
	Script     func(major int, pkgs []string) string
	Parse      func(out string) (mflow.Resolved, error)
	Dockerfile func(base string, major int, arch string, rs []mflow.Recipe, pinned map[string]string) (string, error)
	Tag        func(base, arch, dockerfile string) string
	// Helper is the major-change helper's Dockerfile (lane A3's).
	Helper func(from, to mflow.ExtBuild, arch string, extra ...mflow.Recipe) (string, error)
}

var defaultPGFns = pgRecipeFns{
	Check: mflow.CheckRecipe, Resolve: mflow.ResolveRecipes, Preload: mflow.DerivedPreload,
	Script: mflow.ResolveScript, Parse: mflow.ParseResolve, Dockerfile: mflow.RecipeDockerfile, Tag: mflow.BuildTag, Helper: mflow.HelperDockerfile,
}

// pgFnsFor holds the per-Orchestrator tables tests install.
var pgFnsFor sync.Map

func (o *Orchestrator) pgfn() pgRecipeFns {
	if v, ok := pgFnsFor.Load(o); ok {
		return v.(pgRecipeFns)
	}
	return defaultPGFns
}

// ---- recipes ----

// pgRecipeDefs are the recipes by name: the built-ins, with an admin row that
// carries a definition added or replacing one. A bare row (a switch-off) adds
// no definition.
func pgRecipeDefs(rows []store.PGRecipe) []mflow.Recipe {
	out := slices.Clone(mflow.BuiltinRecipes)
	for _, row := range rows {
		var r mflow.Recipe
		if json.Unmarshal([]byte(row.Spec), &r) != nil || (len(r.Packages) == 0 && r.PackageURL == "") {
			continue
		}
		r.Name = row.ID
		if i := slices.IndexFunc(out, func(x mflow.Recipe) bool { return x.Name == r.Name }); i >= 0 {
			out[i] = r
		} else {
			out = append(out, r)
		}
	}
	return out
}

// pgRecipesFor are the recipe names that list asks for: the first recipe whose
// Extensions hold each name. Names no recipe has are the image's own (contrib).
func pgRecipesFor(defs []mflow.Recipe, exts []string) []string {
	var out []string
	for _, e := range exts {
		if i := slices.IndexFunc(defs, func(r mflow.Recipe) bool { return slices.Contains(r.Extensions, e) }); i >= 0 && !slices.Contains(out, defs[i].Name) {
			out = append(out, defs[i].Name)
		}
	}
	slices.Sort(out)
	return out
}

// pgRowsKeeping returns rows where the named recipes are on and shared with
// everyone: a switched-off or unshared recipe blocks new enables only, a
// rebuild of an instance that already has it resolves anyway.
func pgRowsKeeping(rows []store.PGRecipe, keep []string) []store.PGRecipe {
	out := slices.Clone(rows)
	for i, r := range out {
		if slices.Contains(keep, r.ID) {
			out[i].Enabled, out[i].Share = true, store.NameList{"all"}
		}
	}
	return out
}

// pgAptPackages are the packages of rs from apt, with {major} expanded.
func pgAptPackages(rs []mflow.Recipe, major int) []string {
	var out []string
	for _, r := range rs {
		for _, p := range r.Packages {
			p = strings.ReplaceAll(p, "{major}", fmt.Sprint(major))
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

func pgNames(rs []mflow.Recipe) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

// pgTagOf is the tag of an image ref.
func pgTagOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

var pgMinorRE = regexp.MustCompile(`^(\d+\.\d+)`)

// PGRecipes lists the built-in and the admin-added recipes.
func (o *Orchestrator) PGRecipes(ctx context.Context) ([]PGRecipeView, error) {
	rows, err := o.store.PGRecipes.List(ctx)
	if err != nil {
		return nil, err
	}
	by := map[string]store.PGRecipe{}
	for _, r := range rows {
		by[r.ID] = r
	}
	var out []PGRecipeView
	for _, r := range pgRecipeDefs(rows) {
		v := PGRecipeView{
			Name: r.Name, Enabled: true, Share: []string{"all"}, Majors: r.Majors, Packages: r.Packages,
			PackageURL: r.PackageURL, SHA256: r.SHA256, Version: r.Version, Extensions: r.Extensions,
			Preload: r.Preload, Requires: r.Requires,
		}
		v.Builtin = slices.ContainsFunc(mflow.BuiltinRecipes, func(b mflow.Recipe) bool { return b.Name == r.Name })
		if row, ok := by[r.Name]; ok {
			v.Enabled, v.Share = row.Enabled, slices.Clone([]string(row.Share))
		}
		out = append(out, v)
	}
	return out, nil
}

// SetPGRecipe adds or rewrites an admin recipe, or switches a built-in on or
// off (a built-in keeps its definition: only Enabled and Share are taken).
func (o *Orchestrator) SetPGRecipe(ctx context.Context, by *Principal, v PGRecipeView) (PGRecipeView, error) {
	// by is nil for the server file's own apply, which was approved already.
	if by != nil {
		if err := authz.Can(by.Access, "serverdefaults.set", authz.Resource{}); err != nil {
			return PGRecipeView{}, err
		}
	}
	row := store.PGRecipe{ID: v.Name, Spec: "{}", Enabled: v.Enabled, Share: store.NameList(v.Share)}
	if !slices.ContainsFunc(mflow.BuiltinRecipes, func(b mflow.Recipe) bool { return b.Name == v.Name }) {
		r := mflow.Recipe{
			Name: v.Name, Majors: v.Majors, Packages: v.Packages, PackageURL: v.PackageURL, SHA256: v.SHA256,
			Version: v.Version, Extensions: v.Extensions, Preload: v.Preload, Requires: v.Requires,
		}
		if err := o.pgfn().Check(r); err != nil {
			return PGRecipeView{}, err
		}
		for _, m := range r.Majors {
			if _, ok := mflow.PGReleaseOf(m); !ok {
				var sup []string
				for _, pr := range mflow.PGReleases {
					sup = append(sup, strconv.Itoa(pr.Major))
				}
				return PGRecipeView{}, errs.Invalidf("majors", "%d is not supported. Use %s.", m, strings.Join(sup, ", "))
			}
		}
		if len(r.Requires) > 0 {
			rows, err := o.store.PGRecipes.List(ctx)
			if err != nil {
				return PGRecipeView{}, err
			}
			known := pgRecipeDefs(rows)
			for _, n := range r.Requires {
				if !slices.ContainsFunc(known, func(k mflow.Recipe) bool { return k.Name == n }) {
					return PGRecipeView{}, errs.Invalidf("requires", "no recipe %s", n)
				}
			}
		}
		b, _ := json.Marshal(r)
		row.Spec = string(b)
	}
	if err := o.pgRecipeWrite(ctx, by, "pg.recipe-set", v.Name, func(tx store.Tx) error { return tx.PGRecipes.Upsert(ctx, row) }); err != nil {
		return PGRecipeView{}, err
	}
	all, err := o.PGRecipes(ctx)
	if err != nil {
		return PGRecipeView{}, err
	}
	for _, r := range all {
		if r.Name == v.Name {
			return r, nil
		}
	}
	return PGRecipeView{}, errs.ErrNotFound
}

// DeletePGRecipe removes an admin recipe, or a built-in's switch-off row
// (message 22).
func (o *Orchestrator) DeletePGRecipe(ctx context.Context, by *Principal, name string) error {
	if by != nil {
		if err := authz.Can(by.Access, "serverdefaults.set", authz.Resource{}); err != nil {
			return err
		}
	}
	if slices.ContainsFunc(mflow.BuiltinRecipes, func(b mflow.Recipe) bool { return b.Name == name }) {
		return errs.Conflictf("%s is built in; switch it off instead.", name)
	}
	ms, err := o.store.OperatedInstances.ListEngine(ctx, "postgres")
	if err != nil {
		return err
	}
	n := 0
	for _, m := range ms {
		if eb, _ := mflow.ParseExtBuild(m.ExtBuild); eb.Built() && slices.Contains(eb.Recipes, name) {
			n++
		}
	}
	if n > 0 {
		return errs.Conflictf("%s is enabled on %d instances; it stays until they drop it.", name, n)
	}
	return o.pgRecipeWrite(ctx, by, "pg.recipe-delete", name, func(tx store.Tx) error { return tx.PGRecipes.Delete(ctx, name) })
}

// pgRecipeWrite runs write with its audit event, as the server defaults do;
// the server file's own apply (by nil) is audited by its apply.
func (o *Orchestrator) pgRecipeWrite(ctx context.Context, by *Principal, action, name string, write func(store.Tx) error) error {
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := write(tx); err != nil || by == nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: action, ActorID: by.User.ID, KeyID: by.KeyID, TargetKind: "pg-recipe", TargetID: name, Detail: "recipe " + name,
		})
	})
}

// ---- the plan ----

// pgExtPlanReq is a plan job's payload: the instance list to plan for (nil =
// the current one) and whether the plan moves the base to the newest minor.
type pgExtPlanReq struct {
	Extensions []string
	Update     bool
}

// PlanPGExtensions queues the plan job of a recipe change of an instance and
// returns it; the plan lands under pgextplan.<instance id>. Only Extensions of
// in is read: the image and startup modules of a recipe build are stackr's.
func (o *Orchestrator) PlanPGExtensions(ctx context.Context, by *Principal, tileID string, in PGExtIn) (Job, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return Job{}, err
	}
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return Job{}, err
	}
	if err := mayWrite(by, st); err != nil {
		return Job{}, err
	}
	if m.Engine != "postgres" {
		return Job{}, errs.Invalidf("engine", "%s has no extensions; only postgres does", m.Engine)
	}
	if in.Image != nil || in.Preload != nil {
		return Job{}, errs.Invalidf("extensions", "a plan sets extensions only; the image and startup modules come from the recipes")
	}
	req := pgExtPlanReq{Extensions: slices.Clone([]string(m.Extensions))}
	if in.Extensions != nil {
		if err := deploy.CheckExtNames(*in.Extensions); err != nil {
			return Job{}, err
		}
		req.Extensions = slices.Clone(*in.Extensions)
	}
	return o.queuePGPlan(ctx, t, req)
}

func (o *Orchestrator) queuePGPlan(ctx context.Context, t Tile, req pgExtPlanReq) (Job, error) {
	lock, err := o.pgExtLocks(ctx, t)
	if err != nil {
		return Job{}, err
	}
	return o.enqueue(ctx, kindPGExt, pgExtJob{TileID: t.ID, Plan: &req}, lock...)
}

// LastPGExtPlan is the instance's stored plan, nil when it has none (V10:
// exported so handlers and the drawer read it).
func (o *Orchestrator) LastPGExtPlan(ctx context.Context, instanceID string) (*PGExtPlan, error) {
	v, ok, err := o.store.Settings.Get(ctx, pgextplanKey+instanceID)
	if err != nil || !ok {
		return nil, err
	}
	var p PGExtPlan
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		return nil, errs.Conflictf("the stored plan of %s is unreadable; plan again.", instanceID)
	}
	return &p, nil
}

// LastPGExtPlanOf is LastPGExtPlan by tile id, for callers that hold the
// tile (the plan is stored under the operated instance).
func (o *Orchestrator) LastPGExtPlanOf(ctx context.Context, tileID string) (*PGExtPlan, error) {
	_, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return nil, err
	}
	return o.LastPGExtPlan(ctx, m.ID)
}

// pgNodeArch is the CPU type of a node (the panel's own when it cannot ask).
func (o *Orchestrator) pgNodeArch(ctx context.Context, nodeID string) string {
	if n, err := o.store.Nodes.Get(ctx, nodeID); err == nil {
		if f := o.reportedFacts(ctx, n); f != nil && f.Arch != "" {
			return f.Arch
		}
	}
	return runtime.GOARCH
}

// pgBaseOf is the official image an instance's build sits on: the recorded
// base of a recipe build, else its own official image.
func pgBaseOf(t Tile, eb mflow.ExtBuild) string {
	if eb.Built() {
		return eb.Base
	}
	return cmp.Or(t.ImageRef, mflow.Engines["postgres"].Definition().Image)
}

// pgPlanHash is the plan's hash over live state (R21, V5): the requested
// names, the recipes as they are now, the plan's pins and base, the node and
// CPU type as they are now, and the instance's own columns.
func (o *Orchestrator) pgPlanHash(ctx context.Context, t Tile, m OperatedInstance, in PGExtPlanIn) (string, error) {
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return "", err
	}
	var rs []mflow.Recipe
	if len(in.Names) > 0 {
		if rs, err = o.pgResolve(ctx, t, m, in.Names); err != nil {
			return "", err
		}
	}
	return mflow.PlanHash(mflow.PlanHashIn{
		Names: in.Names, Recipes: rs, Pins: in.Pins, Base: in.Base, Arch: o.pgNodeArch(ctx, node), NodeID: node,
		ImageRef: t.ImageRef, Preload: m.Preload, Extensions: m.Extensions, ExtBuild: m.ExtBuild,
	}), nil
}

// pgResolve resolves recipe names for the instance's major and org; the
// recipes the instance already holds resolve even when switched off.
func (o *Orchestrator) pgResolve(ctx context.Context, t Tile, m OperatedInstance, names []string) ([]mflow.Recipe, error) {
	return o.pgResolveAt(ctx, t, m, names, m.Major)
}

// pgResolveAt is pgResolve for another major (a major change's target).
func (o *Orchestrator) pgResolveAt(ctx context.Context, t Tile, m OperatedInstance, names []string, major int) ([]mflow.Recipe, error) {
	orgID, err := o.tileOrg(ctx, t)
	if err != nil {
		return nil, err
	}
	rows, err := o.store.PGRecipes.List(ctx)
	if err != nil {
		return nil, err
	}
	eb, _ := mflow.ParseExtBuild(m.ExtBuild)
	return o.pgfn().Resolve(names, major, orgID, pgRowsKeeping(rows, eb.Recipes))
}

// pgOneShot runs script in image on node and returns its output.
func (o *Orchestrator) pgOneShot(ctx context.Context, node, name, image, script string, env []string, wait time.Duration) (string, int, error) {
	spec := docker.ContainerSpec{
		Name: name, Image: image, Restart: "no", Cmd: []string{"sh", "-c", script}, Env: env,
		Labels: map[string]string{mflow.LabelPGBootCheck: name},
	}
	return o.oneShotFor(ctx, node, spec, nil, wait, 2000)
}

// pgResolveOn runs the resolver in base: what installs there under hold.
func (o *Orchestrator) pgResolveOn(ctx context.Context, node, base, instID string, major int, pkgs []string) (mflow.Resolved, error) {
	out, code, err := o.pgOneShot(ctx, node, "stackr-extresolve-"+instID[:min(8, len(instID))], base, o.pgfn().Script(major, pkgs), nil, 10*time.Minute)
	if err != nil {
		return mflow.Resolved{}, err
	}
	if code != 0 {
		return mflow.Resolved{}, fmt.Errorf("resolving packages in %s failed:\n%s", base, out)
	}
	return o.pgfn().Parse(out)
}

// planPGExt is the plan job: resolve, price the change, store the plan.
func (o *Orchestrator) planPGExt(ctx context.Context, t Tile, m OperatedInstance, req pgExtPlanReq, log io.Writer) error {
	exts := req.Extensions
	if exts == nil {
		exts = slices.Clone([]string(m.Extensions))
	}
	rows, err := o.store.PGRecipes.List(ctx)
	if err != nil {
		return err
	}
	eb, _ := mflow.ParseExtBuild(m.ExtBuild)
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	p := PGExtPlan{ID: uuid.NewString(), In: PGExtPlanIn{
		Extensions: exts, Names: pgRecipesFor(pgRecipeDefs(rows), exts), Arch: o.pgNodeArch(ctx, node), NodeID: node,
		Update: req.Update, Image: t.ImageRef,
	}}
	block := func(err error) { p.Blockers = append(p.Blockers, err.Error()) }
	base := pgBaseOf(t, eb)
	if !mflow.PGOfficial(base) {
		block(errs.Conflictf("%s runs a custom image; recipes need the official one.", t.Slug))
		return o.storePGPlan(ctx, m, p)
	}
	if req.Update {
		nb, ok := mflow.PGUpdate(base)
		if !ok {
			return errs.Conflictf("already on the newest minor stackr offers")
		}
		base = nb
	}
	p.In.Base = base
	if len(m.Preload) > 0 && !eb.Built() && len(p.In.Names) > 0 {
		block(errs.Conflictf("the startup modules of %s come from its recipes; remove preload, or set a custom image.", t.Slug))
	}
	var rs []mflow.Recipe
	if len(p.In.Names) > 0 {
		if rs, err = o.pgResolve(ctx, t, m, p.In.Names); err != nil {
			block(err) // messages 15 to 17
		}
	}
	if len(p.Blockers) == 0 && len(rs) > 0 {
		if err := o.planPGResolvePins(ctx, t, m, &p, rs, node, log); err != nil {
			return err
		}
	}
	if len(p.Blockers) == 0 {
		o.planPGImage(ctx, t, m, eb, &p, rs, log)
	}
	if err := o.planPGLines(ctx, t, m, eb, rows, &p, rs); err != nil {
		return err
	}
	return o.storePGPlan(ctx, m, p)
}

// planPGResolvePins finds the newest build of every package that installs
// under hold, on the base; when a package needs a newer Postgres it resolves a
// second time on the newest minor stackr offers (R26): messages 18a and 18b.
func (o *Orchestrator) planPGResolvePins(ctx context.Context, t Tile, m OperatedInstance, p *PGExtPlan, rs []mflow.Recipe, node string, log io.Writer) error {
	pkgs := pgAptPackages(rs, m.Major)
	if len(pkgs) == 0 {
		return nil
	}
	res, err := o.pgResolveOn(ctx, node, p.In.Base, m.ID, m.Major, pkgs)
	if err != nil {
		return err
	}
	if res.NeedMinor != "" {
		names := strings.Join(p.In.Names, ", ")
		nb, newer := mflow.PGUpdate(p.In.Base)
		if !newer || p.In.Update {
			p.Blockers = append(p.Blockers, fmt.Sprintf("%s needs postgres %s, which this stackr does not offer yet. Update stackr.", names, res.NeedMinor))
			return nil
		}
		res2, err := o.pgResolveOn(ctx, node, nb, m.ID, m.Major, pkgs)
		if err != nil {
			return err
		}
		if res2.NeedMinor != "" {
			p.Blockers = append(p.Blockers, fmt.Sprintf("%s needs postgres %s, which this stackr does not offer yet. Update stackr.", names, res.NeedMinor))
			return nil
		}
		p.PGUpdate = fmt.Sprintf("%s needs postgres %s. %s runs %s and is updated too.", names, res.NeedMinor, t.Slug, pgTagOf(p.In.Base))
		p.In.Base, res = nb, res2
	}
	p.In.Pins = maps.Clone(res.Versions)
	return nil
}

// planPGImage works out the image the change ends on and the startup modules:
// the build's ref, or the official base when the recipes are all gone.
func (o *Orchestrator) planPGImage(ctx context.Context, t Tile, m OperatedInstance, eb mflow.ExtBuild, p *PGExtPlan, rs []mflow.Recipe, log io.Writer) {
	switch {
	case len(rs) > 0:
		fn := o.pgfn()
		df, err := fn.Dockerfile(p.In.Base, m.Major, p.In.Arch, rs, p.In.Pins)
		if err != nil {
			p.Blockers = append(p.Blockers, err.Error())
			return
		}
		ref, err := o.pgBuildRef(ctx, t, fn.Tag(p.In.Base, p.In.Arch, df))
		if err != nil {
			p.Blockers = append(p.Blockers, err.Error())
			return
		}
		p.In.Image, p.In.Preload = ref, fn.Preload(rs)
	case eb.Built():
		p.In.Image, p.In.Preload = eb.Base, nil
	default:
		p.In.Image, p.In.Preload = t.ImageRef, slices.Clone([]string(m.Preload))
	}
}

// planPGLines prices the plan: the build, the version changes, the Postgres
// update, the restart (message 14) and the refusals the apply would meet.
func (o *Orchestrator) planPGLines(ctx context.Context, t Tile, m OperatedInstance, eb mflow.ExtBuild, rows []store.PGRecipe, p *PGExtPlan, rs []mflow.Recipe) error {
	p.Restart = p.In.Image != t.ImageRef || !slices.Equal(p.In.Preload, []string(m.Preload))
	p.Build = len(rs) > 0 && p.In.Image != t.ImageRef
	if p.Build {
		for _, name := range slices.Sorted(maps.Keys(p.In.Pins)) {
			if from := eb.Packages[name]; from != p.In.Pins[name] {
				p.Versions = append(p.Versions, PGPkgChange{Name: name, From: from, To: p.In.Pins[name]})
			}
		}
		if eb.Built() {
			defs := pgRecipeDefs(rows)
			var old []mflow.Recipe
			for _, d := range defs {
				if slices.Contains(eb.Recipes, d.Name) {
					old = append(old, d)
				}
			}
			for _, name := range pgAptPackages(old, m.Major) {
				if _, kept := p.In.Pins[name]; !kept && eb.Packages[name] != "" {
					p.Versions = append(p.Versions, PGPkgChange{Name: name, From: eb.Packages[name]})
				}
			}
		}
	}
	ss, err := o.slicesOn(ctx, t, m)
	if err != nil {
		return err
	}
	for _, s := range ss {
		p.Slices = append(p.Slices, s.Slug)
	}
	slices.Sort(p.Slices)
	switch {
	case p.Build:
		p.Lines = append(p.Lines, fmt.Sprintf("Build an image for %s with %s on %s.", t.Slug, strings.Join(pgNames(rs), ", "), p.In.Base))
	case p.Restart:
		p.Lines = append(p.Lines, fmt.Sprintf("%s goes back to %s.", t.Slug, p.In.Image))
	default:
		p.Lines = append(p.Lines, "The extension list changes. No build, no restart.")
	}
	for _, c := range p.Versions {
		switch {
		case c.From == "":
			p.Lines = append(p.Lines, fmt.Sprintf("%s %s", c.Name, c.To))
		case c.To == "":
			p.Lines = append(p.Lines, fmt.Sprintf("%s %s is removed", c.Name, c.From))
		default:
			p.Lines = append(p.Lines, fmt.Sprintf("%s %s to %s", c.Name, c.From, c.To))
		}
	}
	if p.PGUpdate != "" {
		p.Lines = append(p.Lines, p.PGUpdate)
	}
	if p.Restart {
		list := "none"
		if len(p.Slices) > 0 {
			list = strings.Join(p.Slices, ", ")
		}
		p.Lines = append(p.Lines, fmt.Sprintf("%s restarts. A startup module runs in every database; a crash in it restarts every slice. Slices on it: %s.", t.Slug, list))
	}
	if len(p.Blockers) > 0 {
		return nil
	}
	// the refusals the apply would meet, said now
	in := p.pgExtIn()
	if _, _, err := o.pgExtPrepare(ctx, t, m, in); err != nil {
		p.Blockers = append(p.Blockers, err.Error())
	}
	return nil
}

// pgExtIn is the request the plan applies: its image, startup modules and
// list, and the build record.
func (p PGExtPlan) pgExtIn() PGExtIn {
	img, pre, exts := p.In.Image, slices.Clone(p.In.Preload), slices.Clone(p.In.Extensions)
	return PGExtIn{Image: &img, Preload: &pre, Extensions: &exts, viaPlan: true}
}

// storePGPlan hashes the plan against live state and stores it.
func (o *Orchestrator) storePGPlan(ctx context.Context, m OperatedInstance, p PGExtPlan) error {
	t, m, err := o.instanceOf(ctx, m.TileID)
	if err != nil {
		return err
	}
	if p.Hash, err = o.pgPlanHash(ctx, t, m, p.In); err != nil && len(p.Blockers) == 0 {
		return err
	}
	b, _ := json.Marshal(p)
	return o.store.Settings.Set(ctx, pgextplanKey+m.ID, string(b))
}

// pgBuildRef is the cluster registry ref a built image gets.
func (o *Orchestrator) pgBuildRef(ctx context.Context, t Tile, tag string) (string, error) {
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return "", err
	}
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return "", err
	}
	if tag == "" {
		return "", errors.New("the recipe engine gave no build tag")
	}
	return image.ClusterRegistry + regfront.Repo(og.Slug, st.Slug, t.Slug) + ":" + tag, nil
}

// ---- the apply ----

// ApplyPGExtPlan applies the plan planID of the instance on tileID. It
// recomputes the hash from live state; a mismatch or a missing plan is message
// 28 and queues a new plan. A plan that restarts needs opts.Confirm (the house
// approve contract) and is refused, not replanned, without it (V6).
func (o *Orchestrator) ApplyPGExtPlan(ctx context.Context, by *Principal, tileID, planID string, opts ApproveOpts) (PGExtView, *Job, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PGExtView{}, nil, err
	}
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return PGExtView{}, nil, err
	}
	if err := mayWrite(by, st); err != nil {
		return PGExtView{}, nil, err
	}
	stale := func(p *PGExtPlan) (PGExtView, *Job, error) {
		req := pgExtPlanReq{}
		if p != nil {
			req = pgExtPlanReq{Extensions: p.In.Extensions, Update: p.In.Update}
		}
		_, _ = o.queuePGPlan(ctx, t, req) // best effort: the error below is the answer
		return PGExtView{}, nil, errs.Conflictf("this plan is out of date; a new one is being made. Review it and apply again.")
	}
	p, err := o.LastPGExtPlan(ctx, m.ID)
	if err != nil {
		return PGExtView{}, nil, err
	}
	if p == nil || p.ID != planID {
		return stale(p)
	}
	mu := o.pgLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	if t, m, err = o.instanceOf(ctx, tileID); err != nil {
		return PGExtView{}, nil, err
	}
	if h, err := o.pgPlanHash(ctx, t, m, p.In); err != nil || h != p.Hash {
		return stale(p)
	}
	pj, _ := json.Marshal(p.approvalPlan())
	if err := checkApprove(p.ID, string(pj), opts); err != nil {
		return PGExtView{}, nil, err
	}
	in, err := o.pgApplyIn(ctx, t, m, p)
	if err != nil {
		return PGExtView{}, nil, err
	}
	if _, j, err := o.setPGExtLocked(ctx, t, m, in); err != nil {
		return PGExtView{}, nil, err
	} else if v, verr := o.PGExtensions(ctx, tileID); verr != nil {
		return v, j, verr
	} else {
		o.delPGPlan(ctx, m.ID)
		return v, j, nil
	}
}

func (o *Orchestrator) pgResolveFor(ctx context.Context, t Tile, m OperatedInstance, names []string) ([]mflow.Recipe, error) {
	if len(names) == 0 {
		return nil, nil
	}
	return o.pgResolve(ctx, t, m, names)
}

func (o *Orchestrator) delPGPlan(ctx context.Context, instanceID string) {
	_ = o.store.Settings.Delete(ctx, pgextplanKey+instanceID)
}

// pgPlanBuild is the build record a plan will leave: the packages are the
// plan's pins until a real build records what dpkg holds. The previous build
// stays as prev_ref for the keep-set.
func (o *Orchestrator) pgPlanBuild(in PGExtPlanIn, rs []mflow.Recipe, old mflow.ExtBuild) mflow.ExtBuild {
	eb := mflow.ExtBuild{Base: in.Base, Recipes: pgNames(rs), Packages: maps.Clone(in.Pins), Arch: in.Arch, Ref: in.Image, PrevRef: old.PrevRef, PrevAt: old.PrevAt}
	if old.Built() && old.Ref != in.Image {
		eb.PrevRef, eb.PrevAt = old.Ref, time.Now().UTC()
	}
	return eb
}

// ---- the build ----

// pgRecipeCheckScript is the recipe boot check: POSIX sh as postgres, a real
// initdb, the startup modules in the flags, then CREATE EXTENSION for every
// extension of the recipes (one psql per statement, ON_ERROR_STOP: Stage 0
// spike 6, apt success proves nothing). It prints FAILED <name> and exits 3
// at the first one that does not create, else the available extensions
// (X lines), the installed postgresql packages (P lines) and the server
// version (S line). Names arrive in the environment, pattern-checked.
const pgRecipeCheckScript = `set -e
B=$(pg_config --bindir 2>/dev/null || true)
if [ -n "$B" ]; then PATH="$B:$PATH"; fi
D=$(mktemp -d /tmp/bc.XXXXXX)
trap 'pg_ctl -D "$D" -m immediate stop >/dev/null 2>&1 || true; rm -rf "$D"' EXIT
initdb -D "$D" -U bc --auth=trust >/dev/null
if [ -n "$CONF" ]; then printf '%s\n' "$CONF" >> "$D/postgresql.conf"; fi
pg_ctl -D "$D" -w -t 60 -o "$FLAGS -c listen_addresses= -k $D -c fsync=off" -l "$D/log" start >/dev/null || { tail -n 40 "$D/log"; exit 1; }
for n in $CREATE; do
  psql -v ON_ERROR_STOP=1 -h "$D" -U bc -d postgres -c "CREATE EXTENSION IF NOT EXISTS \"$n\" CASCADE" >/dev/null || { echo "FAILED $n"; exit 3; }
done
dpkg-query -W -f='P\t${Package}\t${Version}\n' 'postgresql-*' || true
printf 'S\t%s\n' "$(postgres --version)"
psql -v ON_ERROR_STOP=1 -h "$D" -U bc -d postgres -tA -c "SELECT 'X' || chr(9) || name || chr(9) || default_version FROM pg_available_extensions ORDER BY name"
`

// pgWalkScript prints, for each NAME=VERSION in PKGS, the next older build
// that installs under hold in the base image (W lines; empty = none).
const pgWalkScript = `set -e
apt-mark hold "postgresql-$MAJOR" "postgresql-client-$MAJOR" >/dev/null
apt-get update -qq >/dev/null
for kv in $PKGS; do
  p=${kv%%=*}; cur=${kv#*=}; found=
  for v in $(apt-cache madison "$p" | awk '{print $3}'); do
    if dpkg --compare-versions "$v" lt "$cur" && apt-get install -s "$p=$v" >/dev/null 2>&1; then found=$v; break; fi
  done
  printf 'W\t%s\t%s\n' "$p" "$found"
done
`

// pgBuilt is a recipe check's result.
type pgBuilt struct {
	Pkgs   map[string]string
	Server string
	Avail  map[string]string
	Failed string
}

func parsePGCheck(out string) pgBuilt {
	b := pgBuilt{Pkgs: map[string]string{}}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		switch f := strings.Split(l, "\t"); {
		case strings.HasPrefix(l, "FAILED "):
			b.Failed = strings.TrimPrefix(l, "FAILED ")
		case len(f) == 3 && f[0] == "P" && f[2] != "": // dpkg lists packages it only knows of with no version
			b.Pkgs[f[1]] = f[2]
		case len(f) == 2 && f[0] == "S":
			if i := strings.Index(f[1], ") "); i >= 0 {
				b.Server = strings.Fields(f[1][i+2:])[0]
			}
		}
	}
	b.Avail, _ = mflow.ParseBootCheck(out)
	return b
}

// pgBuildImage builds the plan's image on the instance's node and runs the
// recipe boot check on it. A check that fails at an extension walks the failing
// recipe's packages down one build and tries again, up to pgBuildWalkMax times,
// then fails naming the extension. On success st.New carries the built ref, the
// build record with the installed versions and the startup modules.
func (o *Orchestrator) pgBuildImage(ctx context.Context, t Tile, m OperatedInstance, st *pgExtState, log io.Writer) error {
	in := *st.Build
	fn := o.pgfn()
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	rs, err := o.pgResolve(ctx, t, m, in.Names)
	if err != nil {
		return err
	}
	old, _ := mflow.ParseExtBuild(st.Old.ExtBuild)
	pins := maps.Clone(in.Pins)
	var create []string
	for _, r := range rs {
		create = append(create, r.Extensions...)
	}
	preload := fn.Preload(rs)
	for step := 0; ; step++ {
		df, err := fn.Dockerfile(in.Base, m.Major, in.Arch, rs, pins)
		if err != nil {
			return err
		}
		ref, err := o.pgBuildRef(ctx, t, fn.Tag(in.Base, in.Arch, df))
		if err != nil {
			return err
		}
		logf(log, "building %s\n", ref)
		if err := o.pgBuild(ctx, t, node, ref, df, rs, log); err != nil {
			return err
		}
		b, out, code, err := o.pgRecipeCheck(ctx, node, m, ref, preload, create)
		if err != nil {
			return err
		}
		if code == 0 {
			if want := pgMinorRE.FindString(pgTagOf(in.Base)); want != "" && b.Server != "" && b.Server != want {
				return fmt.Errorf("the build moved postgres from %s to %s; nothing was changed", want, b.Server)
			}
			for _, n := range st.New.Extensions {
				if _, ok := b.Avail[n]; !ok {
					return fmt.Errorf("the image %s has no extension %s; nothing was changed", ref, n)
				}
			}
			eb := o.pgPlanBuild(in, rs, old)
			eb.Ref = ref
			if len(b.Pkgs) > 0 {
				eb.Packages = b.Pkgs
			}
			if old.Built() && old.Ref != ref {
				eb.PrevRef, eb.PrevAt = old.Ref, time.Now().UTC()
			}
			st.New.Image, st.New.Preload, st.New.ExtBuild = ref, preload, eb.String()
			return nil
		}
		if b.Failed == "" {
			return fmt.Errorf("postgres did not start in %s with startup modules %s; nothing was changed:\n%s", ref, strings.Join(preload, ", "), out)
		}
		down, ok := map[string]string(nil), false
		if step < pgBuildWalkMax {
			down, ok, err = o.pgWalkDown(ctx, node, m, in.Base, rs, b.Failed, pins)
			if err != nil {
				return err
			}
		}
		if !ok {
			return fmt.Errorf("the build of %s did not pass its check: extension %s was not created; nothing was changed:\n%s", strings.Join(pgNames(rs), ", "), b.Failed, out)
		}
		logf(log, "extension %s did not create; trying older builds\n", b.Failed)
		pins = down
	}
}

// pgBuild writes the Dockerfile to a temp dir under the data dir and builds it
// on node into the cluster registry.
func (o *Orchestrator) pgBuild(ctx context.Context, t Tile, node, ref, df string, rs []mflow.Recipe, log io.Writer) error {
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return err
	}
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.cfg.DataDir, 0o755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(o.cfg.DataDir, "pgext-build-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(df), 0o600); err != nil {
		return err
	}
	_, err = o.buildOn(ctx, node, st.OrgID, regfront.Repo(og.Slug, st.Slug, t.Slug), ref, dir, path, nil, log)
	if err != nil && (strings.Contains(err.Error(), "checksum") || strings.Contains(err.Error(), "sha256")) {
		return fmt.Errorf("the file for %s does not match its sha256: %w", strings.Join(pgNames(rs), ", "), err)
	}
	return err
}

// pgRecipeCheck runs the recipe boot check in the built image.
func (o *Orchestrator) pgRecipeCheck(ctx context.Context, node string, m OperatedInstance, ref string, preload, create []string) (pgBuilt, string, int, error) {
	bc := mflow.BootCheck{Name: "stackr-extcheck-" + m.ID[:min(8, len(m.ID))], Image: ref, Major: m.Major, Preload: preload, Conf: pgDottedConf(m.Settings)}
	spec := bc.Spec()
	spec.Cmd = []string{"sh", "-c", pgRecipeCheckScript}
	spec.Env = append(spec.Env, "CREATE="+strings.Join(create, " "))
	out, code, err := o.oneShotFor(ctx, node, spec, nil, 10*time.Minute, 2000)
	if err != nil {
		return pgBuilt{}, "", 0, err
	}
	return parsePGCheck(out), out, code, nil
}

// pgWalkDown moves every apt package of the recipe that owns the failing
// extension one build older. ok is false when nothing could move.
func (o *Orchestrator) pgWalkDown(ctx context.Context, node string, m OperatedInstance, base string, rs []mflow.Recipe, failed string, pins map[string]string) (map[string]string, bool, error) {
	var own []mflow.Recipe
	for _, r := range rs {
		if slices.Contains(r.Extensions, failed) {
			own = append(own, r)
		}
	}
	var kv []string
	for _, p := range pgAptPackages(own, m.Major) {
		if v := pins[p]; v != "" {
			if mflow.CheckPackage(p, m.Major) != nil || mflow.CheckVersion(v) != nil {
				return nil, false, nil
			}
			kv = append(kv, p+"="+v)
		}
	}
	if len(kv) == 0 { // a URL package has no older build to walk to
		return nil, false, nil
	}
	out, code, err := o.pgOneShot(ctx, node, "stackr-extwalk-"+m.ID[:min(8, len(m.ID))], base, pgWalkScript,
		[]string{fmt.Sprintf("MAJOR=%d", m.Major), "PKGS=" + strings.Join(kv, " ")}, 10*time.Minute)
	if err != nil {
		return nil, false, err
	}
	if code != 0 {
		return nil, false, fmt.Errorf("looking for older builds in %s failed:\n%s", base, out)
	}
	next, moved := maps.Clone(pins), false
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Split(strings.TrimRight(l, "\r"), "\t"); len(f) == 3 && f[0] == "W" && f[2] != "" && mflow.CheckVersion(f[2]) == nil {
			if _, ok := pins[f[1]]; ok {
				next[f[1]], moved = f[2], true
			}
		}
	}
	return next, moved, nil
}

// ---- the deploy hook ----

// rebuildOperated is deploy's RebuildOperated hook: it brings back the image
// of a recipe-built tile from the saved image in the backup destination, else
// by building the recorded pins again (message 33 when they no longer
// install). An error wrapping docker.ErrNoClusterImage says there is nothing
// to rebuild, and the deploy fails with its pull error as before.
func (o *Orchestrator) rebuildOperated(ctx context.Context, t store.Tile, log io.Writer) error {
	m, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return docker.ErrNoClusterImage
	}
	eb, _ := mflow.ParseExtBuild(m.ExtBuild)
	if !eb.Built() || eb.Ref != t.ImageRef {
		return docker.ErrNoClusterImage
	}
	if err := o.loadSavedImage(ctx, t, eb, log); err == nil {
		return nil
	} else if log != nil {
		logf(log, "no saved image to load (%v); building %s again\n", err, t.Slug)
	}
	rs, err := o.pgResolve(ctx, t, m, eb.Recipes)
	if err != nil {
		return err
	}
	pins := map[string]string{}
	for _, p := range pgAptPackages(rs, m.Major) {
		if v := eb.Packages[p]; v != "" {
			pins[p] = v
		}
	}
	fn := o.pgfn()
	df, err := fn.Dockerfile(eb.Base, m.Major, eb.Arch, rs, pins)
	if err != nil {
		return err
	}
	ref, err := o.pgBuildRef(ctx, t, fn.Tag(eb.Base, eb.Arch, df))
	if err != nil {
		return err
	}
	if ref != eb.Ref {
		return o.pgNoLongerInstalls(eb, pins)
	}
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	if err := o.pgBuild(ctx, t, node, ref, df, rs, log); err != nil {
		if strings.Contains(err.Error(), "sha256") {
			return err
		}
		return fmt.Errorf("%w (%v)", o.pgNoLongerInstalls(eb, pins), err)
	}
	return nil
}

// pgNoLongerInstalls is message 33 for the first pinned package.
func (o *Orchestrator) pgNoLongerInstalls(eb mflow.ExtBuild, pins map[string]string) error {
	name, ver := strings.Join(eb.Recipes, ", "), ""
	if ks := slices.Sorted(maps.Keys(pins)); len(ks) > 0 {
		name, ver = ks[0], pins[ks[0]]
	}
	return errs.Conflictf("%s %s can no longer be installed. Run Check again to pick current versions.", name, ver)
}

// loadSavedImage loads the image saved with the instance's base backup into
// the node's Docker and puts it back in the cluster registry.
func (o *Orchestrator) loadSavedImage(ctx context.Context, t Tile, eb mflow.ExtBuild, log io.Writer) error {
	v, err := o.dataVolume(ctx, t)
	if err != nil {
		return err
	}
	_, org, _, err := o.volumeFacts(ctx, v.ID)
	if err != nil {
		return err
	}
	d, err := o.pitrDest(ctx, org, v)
	if err != nil {
		return err
	}
	key := backup.ImageKey(org, v.ID, pgTagOf(eb.Ref))
	err = o.backup.LoadImage(ctx, d.ID, key, func(r io.Reader) error { return o.docker.Load(ctx, eb.Ref, r) }, log)
	if err != nil {
		return err
	}
	if o.cfg.FakeHost == "" {
		leave, err := o.regs.Hold(ctx)
		if err != nil {
			return err
		}
		defer leave()
		_, err = o.pushLocal(ctx, eb.Ref)
		return err
	}
	return nil
}

// pgExtPlanFromFile and pgExtApplyFromFile are promote's PGExtPlan and
// PGExtApply hooks: a stack file's recipe change is planned in the plan step
// and applied with the plan id it stored (G2 calls them). The plan is made
// inline (the plan step is not a job), stored like the panel's, and applied
// without a confirm: the file's own approve was the confirm.
func (o *Orchestrator) pgExtPlanFromFile(ctx context.Context, t store.Tile, m store.OperatedInstance, exts []string, log io.Writer) ([]string, string, error) {
	// an instance not built from recipes whose names need none: no plan, the
	// file's plain path (planID "") stores the list
	if !mflow.RecipeBuilt(m.ExtBuild) {
		rows, err := o.store.PGRecipes.List(ctx)
		if err != nil {
			return nil, "", err
		}
		if len(pgRecipesFor(pgRecipeDefs(rows), exts)) == 0 {
			return nil, "", nil
		}
	}
	nctx, err := o.withTileNode(ctx, t.ID)
	if err != nil {
		return nil, "", err
	}
	if err := o.planPGExt(nctx, t, m, pgExtPlanReq{Extensions: slices.Clone(exts)}, log); err != nil {
		return nil, "", err
	}
	p, err := o.LastPGExtPlan(ctx, m.ID)
	if err != nil || p == nil {
		return nil, "", errors.Join(err, errors.New("the plan was not stored"))
	}
	lines := slices.Clone(p.Lines)
	for _, b := range p.Blockers {
		lines = append(lines, "blocked: "+b)
	}
	return lines, p.ID, nil
}

func (o *Orchestrator) pgExtApplyFromFile(ctx context.Context, t store.Tile, planID string, log io.Writer) error {
	t, m, err := o.instanceOf(ctx, t.ID)
	if err != nil {
		return err
	}
	p, err := o.LastPGExtPlan(ctx, m.ID)
	if err != nil {
		return err
	}
	if p == nil || p.ID != planID {
		return errs.Conflictf("this plan is out of date; a new one is being made. Review it and apply again.")
	}
	mu := o.pgLock(m.ID)
	mu.Lock()
	if h, err := o.pgPlanHash(ctx, t, m, p.In); err != nil || h != p.Hash {
		mu.Unlock()
		return errs.Conflictf("this plan is out of date; a new one is being made. Review it and apply again.")
	}
	if len(p.Blockers) > 0 {
		mu.Unlock()
		return errs.Conflictf("This plan is blocked: %s", strings.Join(p.Blockers, "; "))
	}
	in, err := o.pgApplyIn(ctx, t, m, p)
	if err != nil {
		mu.Unlock()
		return err
	}
	c, changed, err := o.pgExtPrepare(ctx, t, m, in)
	if err != nil || !changed {
		mu.Unlock()
		return err
	}
	if c.extsOnly() || !c.running {
		defer mu.Unlock()
		ps, err := o.operated.ByInstance(ctx, m.ID)
		if err != nil {
			return err
		}
		if err := o.pgExtStoreNow(ctx, c, len(ps) == 0); err != nil {
			return err
		}
		o.delPGPlan(ctx, m.ID)
		return nil
	}
	st := pgExtState{Phase: pgExtQueued, Old: c.old, New: c.new, Build: c.build}
	err = o.setPGExtState(ctx, m.ID, st)
	mu.Unlock()
	if err != nil {
		return err
	}
	o.delPGPlan(ctx, m.ID)
	return o.applyPGExt(ctx, t, m, st, log)
}

// pgApplyIn is the request that applies plan p to the instance.
func (o *Orchestrator) pgApplyIn(ctx context.Context, t Tile, m OperatedInstance, p *PGExtPlan) (PGExtIn, error) {
	in := p.pgExtIn()
	eb, _ := mflow.ParseExtBuild(m.ExtBuild)
	rs, err := o.pgResolveFor(ctx, t, m, p.In.Names)
	if err != nil {
		return in, err
	}
	next := "{}"
	if len(rs) > 0 {
		next = o.pgPlanBuild(p.In, rs, eb).String()
	}
	in.extBuild = &next
	if p.Build {
		bq := p.In
		in.build = &bq
	}
	return in, nil
}
