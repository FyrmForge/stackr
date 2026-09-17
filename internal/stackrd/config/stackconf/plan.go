package stackconf

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// normRestart folds the old "always"/"unless-stopped" spellings into the
// canonical "" so a row written before the swarm move does not diff forever
// against a config that says the same thing.
func normRestart(s string) string {
	v, _ := runtime.NormalizeRestart(s)
	return v
}

// State is a snapshot of what a stack currently looks like in the database.
// Building it (DB reads) happens at the caller; diffing stays pure.
type State struct {
	Envs map[string]EnvState // by slug; ephemeral envs must not be included
	// DomainRes is the stack's OWN domain resources (level "stack"). Org and
	// instance rows are inherited, not owned, so the stack file never deletes
	// them. Stack-level, so an env-scoped plan ignores them.
	DomainRes []repo.DomainResource
	// AllDomainRes is every resource on the server, for the uniqueness check.
	AllDomainRes []repo.DomainResource
	// OrgConnectors is the ids of the connectors this stack's org owns. A
	// tile's connector: is a raw id straight out of the file, and the token it
	// mints clones private repositories, so naming another org's id has to be
	// a plan error rather than a silent fallback at deploy time.
	OrgConnectors map[string]bool
	// AllDomains is every tile domain on the server: host+path is unique
	// (idx_domains_host_path), so a claim on a host another tile routes must
	// fail in the plan, not as a constraint error mid-apply.
	AllDomains []repo.Domain
	// StackSlug and Middlewares are the stack row's slug and stored
	// proxy.middlewares. OrgMiddlewares is every other stack's names in the
	// org by slug; MiddlewareUsers is the other stacks' tiles naming one of
	// this stack's, by name.
	StackSlug       string
	Middlewares     string
	OrgMiddlewares  map[string]map[string]bool
	MiddlewareUsers map[string][]string
}

type EnvState struct {
	Tiles map[string]TileState // by slug
	// Color is the env row's own colour, "" when it has none.
	Color string
	// ApexHosts is the visible domain resources' bare hosts, so the
	// serializer can round-trip an apex claim as the intent, not a literal.
	ApexHosts map[string]bool
}

type TileState struct {
	Tile    repo.Tile
	Domains []repo.Domain
	// Slice marks this entry as a live provisioned slice (logical db /
	// bucket) rather than a tile row, slices share the tile slug namespace.
	// Resolved in Snapshot because turning provision rows back into
	// addresses takes store lookups, and Diff is a pure function.
	Slice *SliceState
}

// SliceState is a live slice as the differ sees it.
type SliceState struct {
	Instance     string // the providing instance's slug (identity compare)
	Engine       string // the instance's engine: its SliceName rule normalises the file's name
	InstancePath string // dotted display form, relative where possible
	Name         string // db/bucket name inside the instance
	OnRemove     string // row policy: "" | detach | drop
	Public       bool
}

// Change is one line of a plan.
type Change struct {
	Kind  string `json:"kind"` // create-env | update-env | delete-env | create | delete | update
	Env   string `json:"env"`
	Tile  string `json:"tile,omitempty"`
	Field string `json:"field,omitempty"` // update only
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
	// Note is a consequence the approver has to see before clicking apply,
	// the data loss a db-engine replace causes, a narrowed sharing scope, a
	// dropped slice.
	Note string `json:"note,omitempty"`
	// Fields is what a create row is actually creating, for the plan page's
	// accordion: source, port, hosts, env keys, volumes, dependencies. Names
	// only, never values, the plan is stored in the database and a resolved
	// tile carries secrets.
	Fields []Field `json:"fields,omitempty"`
	// Input marks a row for a value the config declares and nobody has set.
	// The plan page opens it and puts the Set box inside, so a missing value
	// reads as a line of the diff instead of a banner over it.
	Input bool `json:"input,omitempty"`
	// Destroys marks a change that destroys data without deleting a tile,
	// today only a `uses:` entry leaving the file with on_remove: drop. It
	// exists so the gate can key on consequence rather than on Kind, which
	// would otherwise wave a slice drop through the auto-apply path.
	Destroys bool `json:"destroys,omitempty"`
}

// redactURL drops credentials from a clone URL. Fields land in the stored plan
// and on the plan page, and a config may legitimately carry
// https://user:token@host/repo.git.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// Field is one line of a create row's detail.
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Input is a value the config declares and nobody has set. It is not an error:
// the file says the name exists, the apply makes it exist, and the plan page
// offers a box to fill it in. Blocking the apply instead would mean a stack
// could never be stood up from its own config, which is the whole point of
// declaring it there.
type Input struct {
	Scope    string `json:"scope"` // stack | org
	Name     string `json:"name"`
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	// Blocked is the blast radius as "env/tile": the tiles that read the
	// value plus everything downstream of them, none of which will come up
	// until it is set. Empty for org-scope inputs, their readers live in
	// stacks this plan cannot see.
	Blocked []string `json:"blocked,omitempty"`
}

// Plan is the full diff of desired config vs current state.
type Plan struct {
	Changes []Change `json:"changes"`
	Errors  []string `json:"errors,omitempty"`
	// Warnings do not block an apply. A declared secret with no value is the
	// case they exist for: the config is legitimate, the stack simply is not
	// configured yet, and refusing to apply would mean a stack could never be
	// stood up before its credentials were in place. The deploy of a tile that
	// actually reads the missing value still fails, an unresolved reference
	// is never run.
	Warnings []string `json:"warnings,omitempty"`
	// GenSecrets is the `default: generated` secrets that still have no value.
	// Minting them is real work, so they make a plan non-empty: a commit whose
	// only change is a new generated secret used to plan "clean", which the
	// auto-apply path skips, so the secret sat unminted until some unrelated
	// tile change finally dragged an apply along.
	GenSecrets []string `json:"gen_secrets,omitempty"`
	// hostOwner is host+path → tile id for every domain on the server, plus
	// the claims this plan makes as it goes, so two tiles in one file cannot
	// both take a host.
	hostOwner map[string]string
	// Inputs is the declared-but-unset values the plan page can collect before
	// the apply. Rendered as boxes, never as blockers.
	Inputs []Input `json:"inputs,omitempty"`
	// Moves are pinned tiles whose node group the file has changed, whose
	// home node is not in the new group. They block the apply until each one
	// has been moved, because a group change on a pinned tile is a request to
	// put its data on a different machine and nothing but a copy can do that
	// (docs/plans/31-node-agent-open-questions.md, step 8). The plan page
	// renders one row per move with the same Move modal the tile drawer uses.
	Moves []MoveBlock `json:"moves,omitempty"`
}

// MoveBlock is one pinned tile standing between a plan and its apply.
type MoveBlock struct {
	TileID   string `json:"tile_id"`
	Tile     string `json:"tile"`
	Env      string `json:"env"`
	FromNode string `json:"from_node"`
	ToGroup  string `json:"to_group"`
}

// InputChange is the plan row a declared-but-unset value renders as. Env
// carries the scope (stack | org), not an env slug: the value lives at the
// scope, and the row is the same wherever the plan is reviewed.
func InputChange(in Input) Change {
	note := "Needed before the tiles below can deploy. Apply goes ahead without it."
	if in.Required {
		note = "Required. " + note
	}
	ch := Change{Kind: "update", Env: in.Scope, Tile: "secrets", Field: in.Name,
		New: "unset", Input: true, Note: note}
	if len(in.Blocked) > 0 {
		ch.Fields = []Field{{Name: "will not deploy", Value: strings.Join(in.Blocked, ", ")}}
	}
	return ch
}

// GenChange is the row a `default: generated` secret renders as.
func GenChange(scope, name string) Change {
	return Change{Kind: "update", Env: scope, Tile: "secrets", Field: name,
		New: "generate", Note: "minted on apply, stable forever"}
}

// InputsFirst floats the input rows to the top, then the generated ones. What
// the reviewer can act on before approving comes before what the apply does
// on its own.
func (p *Plan) InputsFirst() {
	rank := func(c Change) int {
		switch {
		case c.Input:
			return 0
		case c.New == "generate" && c.Tile == "secrets":
			return 1
		}
		return 2
	}
	sort.SliceStable(p.Changes, func(i, j int) bool { return rank(p.Changes[i]) < rank(p.Changes[j]) })
}

// createFields describes a tile the plan is about to create, in the order the
// accordion shows it. Env is keys only: values go through the same store the
// plan row does, and a plan is not a place to write secrets down.
func createFields(tc TileConf) []Field {
	var out []Field
	add := func(name, val string) {
		if val != "" {
			out = append(out, Field{Name: name, Value: val})
		}
	}
	add("kind", tc.Type)
	add("image", tc.Image)
	if tc.Build != nil {
		add("build", "from repo")
	}
	add("git", redactURL(tc.GitURL))
	add("branch", tc.Branch)
	if tc.Port != 0 {
		add("port", strconv.Itoa(tc.Port))
	}
	add("published ports", tc.PublishedPorts)
	if len(tc.Domains) > 0 {
		var hosts []string
		for _, d := range tc.Domains {
			hosts = append(hosts, d.Host)
		}
		add("hosts", strings.Join(hosts, ", "))
	}
	if len(tc.Env) > 0 {
		keys := make([]string, 0, len(tc.Env))
		for k := range tc.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		add("env", strings.Join(keys, ", "))
	}
	if len(tc.Volumes) > 0 {
		add("volumes", strings.Join(tc.Volumes, ", "))
	}
	if len(tc.DependsOn) > 0 {
		add("depends on", strings.Join(tc.DependsOn, ", "))
	}
	return out
}

// Destructive reports whether applying would delete anything.
func (p *Plan) Destructive() bool {
	for _, c := range p.Changes {
		if c.Kind == "delete" || c.Kind == "delete-env" || c.Destroys {
			return true
		}
	}
	return false
}

func (p *Plan) Empty() bool {
	return len(p.Changes) == 0 && len(p.Errors) == 0 && len(p.GenSecrets) == 0
}

// Declared marks the rows finishPlan adds for values the config declares: one
// nobody has set, one the apply will mint. They are shown, never counted. A
// stack whose only row is an unset secret has nothing to apply, and counting
// it left that stack with a plan pending review forever.
func (c Change) Declared() bool {
	return c.Tile == "secrets" && (c.Input || c.New == "generate")
}

// DiffOpts carries stack-level context the diff needs.
type DiffOpts struct {
	GitURL        string // the bound repo's clone URL, expected on built tiles
	Connector     string // the bound git connector id, fallback for tiles that don't name one
	DefaultBranch string // the bound branch, tile branch fallback

	// Domain-claim context (auto/apex): the slugs that feed generated names
	// and the resources this stack may claim under, nearest level first.
	// Zero-valued in pure unit tests, where only literal hosts appear.
	OrgSlug         string
	StackSlug       string
	DefaultEnv      string
	DomainResources []repo.DomainResource
	// ForeignOrgSlugs is every other organization's slug on this server. A
	// literal host may not start with one: config as code would otherwise be
	// the hole in the anti-squat rule the UI paths enforce.
	ForeignOrgSlugs map[string]bool

	// OnlyEnv restricts the diff to one environment (env-branch plans);
	// env-level create/delete of OTHER envs never appears in such a plan.
	OnlyEnv string
	// SkipEnvs excludes envs bound to their own branch from a stack-branch
	// plan, those are managed by their branch's plans.
	SkipEnvs map[string]bool
}

// OutOfScope reports whether an env slug is outside this plan's scope. The
// same two-line rule half a dozen walks were writing out by hand, which is
// how the move planner came to have neither.
func (o DiffOpts) OutOfScope(envSlug string) bool {
	if o.OnlyEnv != "" && envSlug != o.OnlyEnv {
		return true
	}
	return o.SkipEnvs[envSlug]
}

// claimHost resolves a domain entry to its concrete hostname: literal hosts
// pass through, apex must name a visible resource, auto generates under the
// nearest one.
func claimHost(dc DomainConf, envSlug, tileSlug string, opts DiffOpts) (string, error) {
	switch {
	case dc.Auto:
		if len(opts.DomainResources) == 0 {
			return "", fmt.Errorf("tile %s: auto domain, but no domain resource is visible to this stack; add one at stack, org or server level", tileSlug)
		}
		res := opts.DomainResources[0]
		return envops.AutoHost(res, opts.OrgSlug, opts.StackSlug, envSlug, tileSlug, envSlug == opts.DefaultEnv), nil
	case dc.Apex != "":
		for _, r := range opts.DomainResources {
			if r.Host == dc.Apex {
				return r.Host, nil
			}
		}
		return "", fmt.Errorf("tile %s: apex %q is not a domain resource visible to this stack", tileSlug, dc.Apex)
	}
	if label, _, ok := strings.Cut(strings.TrimPrefix(dc.Host, "*."), "."); ok && opts.ForeignOrgSlugs[label] {
		return "", fmt.Errorf("tile %s: host %q starts with another organization's slug", tileSlug, dc.Host)
	}
	return dc.Host, nil
}

// releasedHosts is the set of "<tile id> <host+path>" rows this plan takes
// away, so a hostname moving from one tile to another is one change instead of
// a conflict with itself.
//
// Deliberately a list of what goes, not of what stays. AllDomains is every
// domain row on the server, other stacks included, and a row this plan cannot
// see is a row that is not moving: it has to keep blocking.
//
// Only envs the plan covers count, and only tiles the file still declares. A
// tile the file has dropped keeps its rows here even though the apply deletes
// it, which is the conservative direction: at worst a plan that both deletes a
// tile and gives its hostname to another one needs two applies.
func releasedHosts(r *Resolved, s State, opts DiffOpts) map[string]bool {
	out := map[string]bool{}
	for envName, es := range s.Envs {
		if (opts.OnlyEnv != "" && envName != opts.OnlyEnv) || opts.SkipEnvs[envName] {
			continue
		}
		re, declared := r.Envs[envName]
		if !declared {
			continue
		}
		for slug, ts := range es.Tiles {
			tc, wanted := re.Tiles[slug]
			if !wanted {
				continue
			}
			keep := map[string]bool{}
			for _, dc := range tc.Domains {
				if host, err := claimHost(dc, envName, slug, opts); err == nil && host != "" {
					keep[host+normPath(dc.Path)] = true
				}
			}
			for _, d := range ts.Domains {
				if key := d.Host + normPath(d.Path); !keep[key] {
					out[ts.Tile.ID+" "+key] = true
				}
			}
		}
	}
	return out
}

// withFileDefault points opts at the file's bottom rung rather than whatever
// environment the store happens to list first.
//
// They are different answers and only one is right. loadDomainContext reads
// the store's first row, which on a stack created by hand is the environment
// the wizard made ("Production"), even when the file declares another one
// first. The default environment is the one whose tiles get the bare generated
// hostname, so getting it wrong makes the wrong environment claim
// site.<stack>.<org>.<domain> and every later plan then fails with "already
// routes to another service".
func withFileDefault(r *Resolved, opts DiffOpts) DiffOpts {
	if r != nil && r.DefaultEnv != "" {
		opts.DefaultEnv = r.DefaultEnv
	}
	return opts
}

// Diff computes the plan: desired (resolved config) vs current (state).
func Diff(r *Resolved, s State, opts DiffOpts) *Plan {
	// Belt and braces: callers should have run withFileDefault, and it is
	// idempotent. This used to be the only place it happened, which was the
	// bug: opts is a value, so the override never reached execute, and the
	// apply then generated hostnames against the store's stale default while
	// the diff had used the file's.
	opts = withFileDefault(r, opts)
	p := &Plan{hostOwner: map[string]string{}}
	// Renames first: without them a moved key is a delete and a create, which
	// for a tile with a volume destroys data. Re-keying the state is what
	// makes the rest of the diff see one object under a new name.
	s = renameState(s, planMoves(r, s, p, opts))
	// Seeded from what exists, minus the rows this plan is about to remove.
	//
	// A host moving between two tiles the same plan touches is one change, not
	// a conflict. Seeding every existing row unconditionally made it a
	// conflict: with staging declared as the bottom rung after production had
	// already taken the bare hostname, the plan that hands it to staging and
	// takes it off production was refused with "already routes to another
	// service", and a stack in that state had no way forward inside the
	// product.
	gone := releasedHosts(r, s, opts)
	for _, d := range s.AllDomains {
		key := d.Host + normPath(d.Path)
		if gone[d.TileID+" "+key] {
			continue
		}
		p.hostOwner[key] = d.TileID
	}

	// The home (shared tiles) goes with the stack-scoped plan, like the rename
	// and domain rows: it has no rung, so no env-scoped plan ever carries it.
	// Its row is the store's to create, never a plan change.
	envs := r.EnvOrder
	if _, ok := r.Envs[repo.HomeSlug]; ok {
		envs = append([]string{repo.HomeSlug}, envs...)
	}
	for _, envName := range envs {
		if opts.OnlyEnv != "" && envName != opts.OnlyEnv {
			continue
		}
		if opts.SkipEnvs[envName] {
			continue
		}
		re := r.Envs[envName]
		cur, envExists := s.Envs[envName]
		switch {
		case envName == repo.HomeSlug:
			// no env row to create or colour
		case !envExists:
			p.Changes = append(p.Changes, Change{Kind: "create-env", Env: envName})
		case re.Color != cur.Color:
			p.Changes = append(p.Changes, Change{Kind: "update-env", Env: envName, Field: "color",
				Old: defStr(cur.Color, "default"), New: defStr(re.Color, "default")})
		}
		for _, tileName := range sortedTileNames(re.Tiles) {
			tc := re.Tiles[tileName]
			p.checkConnector(envName, tileName, tc, s)
			ts, ok := cur.Tiles[tileName]
			if !ok {
				p.claimHosts(envName, tileName, tc, "", opts)
				p.Changes = append(p.Changes, Change{Kind: "create", Env: envName, Tile: tileName,
					New: tc.Type, Fields: createFields(tc)})
				continue
			}
			p.diffTile(envName, tileName, tc, ts, cur, opts)
		}
		// Tiles present in the env but absent from config: strict mode deletes.
		for _, tileName := range sortedStateNames(cur.Tiles) {
			if _, declared := re.Tiles[tileName]; declared {
				continue
			}
			ch := Change{Kind: "delete", Env: envName, Tile: tileName}
			if sl := cur.Tiles[tileName].Slice; sl != nil {
				if sl.OnRemove == "drop" {
					ch.Destroys = true
					ch.Note = "on_remove: drop. The data behind this slice is destroyed, not orphaned"
				} else {
					ch.Note = "the slice is orphaned and its data kept; drop it from the instance's panel to reclaim the space"
				}
			}
			p.Changes = append(p.Changes, ch)
		}
	}
	if opts.OnlyEnv == "" {
		p.diffDomainRes(r.Domains, s)
		p.diffMiddlewares(r, s)
	}
	p.checkMiddlewareRefs(r, s, opts)
	// Static envs not declared: strict mode deletes. Env-scoped plans never
	// judge other envs' existence.
	if opts.OnlyEnv == "" {
		for _, envName := range sortedEnvNames(s.Envs) {
			if opts.SkipEnvs[envName] || envName == repo.HomeSlug {
				continue
			}
			if _, declared := r.Envs[envName]; !declared {
				p.Changes = append(p.Changes, Change{Kind: "delete-env", Env: envName})
			}
		}
	}
	return p
}

// diffDomainRes reconciles the stack's declared domains: with its own rows.
// The changes carry Env "stack" (like the rename), they are not tiles and
// execute never sees them; ApplyPlan takes them out first.
//
// A host another owner already claims is a plan error, not an apply error: the
// host index is unique server-wide, so the insert would fail halfway through an
// apply that has already created containers.
//
// Only rows the file created (Declared) are the file's to delete, the same rule
// orgconf applies to stacks via OrgDeclared. Until domains: existed there was no
// way to declare one, so every pre-existing row is a panel row, deleting those
// on the first apply would drop live hostnames nobody asked to remove, and would
// make every plan destructive enough to stop webhook auto-apply. Declaring a
// panel row in the file adopts it, and the plan says so.
func (p *Plan) diffDomainRes(want []DomainResConf, s State) {
	have := map[string]repo.DomainResource{}
	for _, r := range s.DomainRes {
		have[r.Host] = r
	}
	declared := map[string]bool{}
	for _, d := range want {
		declared[d.Host] = true
		cur, ok := have[d.Host]
		if !ok {
			for _, other := range s.AllDomainRes {
				if other.Host == d.Host {
					p.Errors = append(p.Errors, "domains: "+d.Host+" is already claimed on this server")
				}
			}
			p.Changes = append(p.Changes, Change{Kind: "create", Env: "stack", Field: "domain", New: d.Host})
			continue
		}
		switch {
		case cur.IncludeEnvOnDefault != d.IncludeEnvOnDefault:
			p.Changes = append(p.Changes, Change{Kind: "update", Env: "stack", Field: "domain",
				Old: d.Host + " include_env_on_default=" + boolStr(cur.IncludeEnvOnDefault),
				New: d.Host + " include_env_on_default=" + boolStr(d.IncludeEnvOnDefault)})
		case !cur.Declared:
			p.Changes = append(p.Changes, Change{Kind: "update", Env: "stack", Field: "domain", New: d.Host,
				Note: "added in the panel; the file adopts it, and dropping it from the file will delete it"})
		}
	}
	for _, r := range s.DomainRes {
		if !declared[r.Host] && r.Declared {
			p.Changes = append(p.Changes, Change{Kind: "delete", Env: "stack", Field: "domain", Old: r.Host,
				Note: "generated hostnames already claimed under it keep working until their tile redeploys"})
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// desiredKind maps a config type onto the tile Kind column.
func desiredKind(t string) string {
	switch t {
	case "cron":
		return "cron"
	case "function":
		return "function"
	case "volume":
		return "volume"
	}
	return "service"
}

func (p *Plan) diffTile(env, name string, tc TileConf, ts TileState, cur EnvState, opts DiffOpts) {
	t := ts.Tile

	// Slices diff on their own axis; a slice and a tile trading a slug is a
	// replace like any other type change.
	if tc.Type == "slice" || ts.Slice != nil {
		p.diffSlice(env, name, tc, ts)
		return
	}

	// Type change is a replace.
	if t.Kind != desiredKind(tc.Type) || (tc.Type == "managed") != t.IsManaged() {
		p.Changes = append(p.Changes,
			Change{Kind: "delete", Env: env, Tile: name},
			Change{Kind: "create", Env: env, Tile: name, New: tc.Type, Fields: createFields(tc)})
		return
	}
	// A db engine change is a replace too: writing the column left a running
	// postgres labelled mysql. Replace mints a new tile id and volume names
	// derive from it, so the old data is orphaned, not migrated, say so.
	if tc.Type == "managed" && tc.Engine != "" && t.Engine != tc.Engine {
		note := "engine " + t.Engine + " → " + tc.Engine + " replaces the database; its data volume is left behind, not migrated"
		p.Changes = append(p.Changes,
			Change{Kind: "delete", Env: env, Tile: name, Note: note},
			Change{Kind: "create", Env: env, Tile: name, New: tc.Type, Note: note, Fields: createFields(tc)})
		return
	}

	upd := func(field, old, new_ string) {
		if old != new_ {
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name, Field: field, Old: old, New: new_})
		}
	}

	switch tc.Type {
	case "volume":
		upd("attach", attachedSlug(cur, t.AttachedTileID), tc.Attach)
		upd("path", t.MountPath, tc.Path)
	case "managed":
		// engine is handled above as a replace. The rest is in-place: the port
		// changes the host mapping, the scope changes who may provision from
		// the instance. Both take a redeploy, which updateTile does.
		upd("external_port", strconv.Itoa(t.ExternalPort), strconv.Itoa(tc.ExternalPort))
		upd("shm_size_mb", strconv.Itoa(t.ShmSizeMB), strconv.Itoa(tc.ShmSizeMB))
		upd("replicas", strconv.Itoa(max(t.Replicas, 1)), strconv.Itoa(max(tc.Replicas, 1)))
		upd("node_group", t.NodeGroup, tc.NodeGroup)
		p.blockOnGroupMove(env, name, t, tc.NodeGroup)
		// Absent image: means the engine default, not "leave the pin alone",
		// otherwise an override could never be taken back out of the file.
		def := ""
		if eng, ok := managedtiles.Engines[t.Engine]; ok {
			def = eng.DefaultImage
		}
		upd("image", defStr(t.ImageRef, def), defStr(tc.Image, def))
		if old, want := defStr(t.ScopeKind, "env"), defStr(tc.Scope, "env"); old != want {
			ch := Change{Kind: "update", Env: env, Tile: name, Field: "scope", Old: old, New: want}
			// Narrowing strands whatever is already provisioned from outside the
			// new scope, those tiles keep their credentials and lose the right
			// to be re-provisioned. Worth saying before someone approves it.
			if scopeRank(want) < scopeRank(old) {
				ch.Note = "narrowing scope: tiles outside " + want + " can no longer provision from this instance"
			}
			p.Changes = append(p.Changes, ch)
		}
	case "cron":
		diffSource(upd, t, tc, opts)
		upd("schedule", t.Cron, tc.Schedule)
		upd("command", t.Command, tc.Command)
		upd("allow_overlap", strconv.FormatBool(t.AllowOverlap), strconv.FormatBool(tc.AllowOverlap))
		// Absent timeout_minutes means the 30-minute default, not "leave as is",
		// otherwise a value set once could never be taken back out. Explicit 0 is
		// rejected by Validate.
		upd("timeout_minutes", strconv.Itoa(t.TimeoutMinutes), strconv.Itoa(defInt(tc.TimeoutMinutes, 30)))
	case "function":
		diffSource(upd, t, tc, opts)
		upd("command", t.Command, tc.Command)
		upd("run_on_deploy", strconv.FormatBool(t.RunOnDeploy), strconv.FormatBool(tc.RunOnDeploy))
		upd("allow_overlap", strconv.FormatBool(t.AllowOverlap), strconv.FormatBool(tc.AllowOverlap))
		upd("timeout_minutes", strconv.Itoa(t.TimeoutMinutes), strconv.Itoa(defInt(tc.TimeoutMinutes, 30)))
	case "service":
		diffSource(upd, t, tc, opts)
		upd("port", strconv.Itoa(t.ContainerPort), strconv.Itoa(tc.Port))
		upd("healthcheck", t.HealthcheckCmd, tc.Healthcheck)
		upd("healthcheck_interval", strconv.Itoa(t.HealthcheckIntervalS), strconv.Itoa(tc.HealthInterval))
		upd("healthcheck_timeout", strconv.Itoa(t.HealthcheckTimeoutS), strconv.Itoa(tc.HealthTimeout))
		upd("healthcheck_retries", strconv.Itoa(t.HealthcheckRetries), strconv.Itoa(tc.HealthRetries))
		upd("healthcheck_start_period", strconv.Itoa(t.HealthcheckStartPeriodS), strconv.Itoa(tc.HealthStartPeriod))
		upd("security_headers", strconv.FormatBool(t.SecHeaders), strconv.FormatBool(tc.SecurityHeaders))
		upd("volumes", joinNorm(strings.Split(t.Volumes, "\n")), joinNorm(tc.Volumes))
		upd("watch_paths", joinNorm(strings.Split(t.WatchPaths, "\n")), joinNorm(tc.WatchPaths))
		upd("build_args", t.BuildArgs, tc.BuildArgs)
		upd("published_ports", joinNorm(strings.Split(t.PublishedPorts, "\n")), joinNorm(strings.Split(tc.PublishedPorts, "\n")))
		upd("traefik_override", t.TraefikOverride, tc.TraefikOverride)
		upd("basic_auth_user", t.BasicAuthUser, tc.BasicAuthUser)
		upd("basic_auth_password", t.BasicAuthPassword, tc.BasicAuthPassword)
		upd("command", t.Command, tc.Command)
		upd("user", t.User, tc.User)
		upd("shm_size_mb", strconv.Itoa(t.ShmSizeMB), strconv.Itoa(tc.ShmSizeMB))
		upd("replicas", strconv.Itoa(max(t.Replicas, 1)), strconv.Itoa(max(tc.Replicas, 1)))
		upd("node_group", t.NodeGroup, tc.NodeGroup)
		p.blockOnGroupMove(env, name, t, tc.NodeGroup)
		upd("privileged", strconv.FormatBool(t.Privileged), strconv.FormatBool(tc.Privileged))
		upd("devices", joinNorm(strings.Split(t.Devices, "\n")), joinNorm(tc.Devices))
		upd("restart", normRestart(t.RestartPolicy), normRestart(tc.Restart))
		upd("files", joinNorm(strings.Split(t.Files, "\n")), joinNorm(tc.Files))
		upd("storage", joinNorm(strings.Split(t.Storage, "\n")), joinNorm(tc.Storage))
	}

	switch tc.Type {
	case "service", "cron", "function":
		// joinNorm sorts, right for deps, which are a set (unlike domains,
		// where position picks the primary).
		upd("depends_on", joinNorm(strings.Split(t.DependsOn, "\n")), joinNorm(tc.DependsOn))
		upd("wait_for_ci", strconv.FormatBool(t.WaitForCI), strconv.FormatBool(tc.WaitForCI))
	}
	switch tc.Type {
	case "service", "cron", "function", "managed":
		upd("update_policy", defStr(t.UpdatePolicy, "off"), defStr(tc.UpdatePolicy, "off"))
	}

	// Absent limits: means "no limits", not "don't touch", a one-way ratchet
	// otherwise: once set in the file, removing the block never cleared them.
	var wantCPU float64
	var wantMem int
	if tc.Limits != nil {
		wantCPU, wantMem = tc.Limits.CPU, tc.Limits.MemoryMB
	}
	upd("cpu_limit", trimFloat(t.CPULimit), trimFloat(wantCPU))
	upd("memory_mb", strconv.Itoa(t.MemLimitMB), strconv.Itoa(wantMem))
	if tc.Env != nil {
		// Values never reach the plan UI: an env value may be a credential, and
		// a plan is rendered in the console and posted into PR comments. The
		// diff is on presence and change, which is what a reviewer needs.
		if old, new_ := canonEnv(t.Env, nil), canonEnv(EnvLines(tc.Env), nil); old != new_ {
			upd("env", "", envChangeSummary(old, new_))
		}
	}
	p.diffDomains(env, name, tc, ts, opts)
}

// diffSource compares a runnable tile's source (image / git build), shared by
// every run policy, because the base of a runnable type is the same: a source
// the deploy engine turns into an artifact.
func diffSource(upd func(field, old, new_ string), t repo.Tile, tc TileConf, opts DiffOpts) {
	switch {
	case tc.Image != "":
		upd("source", sourceLabel(&t), "image "+tc.Image)
		upd("git_url", t.GitURL, tc.GitURL)
		upd("branch", t.GitBranch, tc.Branch)
		conn := ""
		if tc.GitURL != "" {
			conn = firstNonEmpty(tc.Connector, opts.Connector)
		}
		upd("connector", t.ConnectorID, conn)
	default:
		branch := tc.Branch
		if branch == "" {
			branch = opts.DefaultBranch
		}
		upd("source_type", t.SourceType, "git")
		if u := firstNonEmpty(tc.GitURL, opts.GitURL); u != "" {
			upd("git_url", t.GitURL, u)
		}
		upd("connector", t.ConnectorID, firstNonEmpty(tc.Connector, opts.Connector))
		upd("branch", t.GitBranch, branch)
		upd("build_context", normDot(t.BuildContext), normDot(buildContext(tc)))
		upd("dockerfile", defStr(t.DockerfilePath, "Dockerfile"), defStr(dockerfile(tc), "Dockerfile"))
	}
}

// diffSlice compares one slice entry against the live slice under the same
// slug. Identity is (instance, name): moving either is a replace, because the
// data does not follow. on_remove is compared like any other field, and it is
// the *held* policy that decides what a removal does, by then the entry that
// declared it is gone, which is why apply persists it on the provision row.
func (p *Plan) diffSlice(env, name string, tc TileConf, ts TileState) {
	deletion := func() Change {
		ch := Change{Kind: "delete", Env: env, Tile: name}
		if ts.Slice.OnRemove == "drop" {
			ch.Destroys = true
			ch.Note = "on_remove: drop. The data behind this slice is destroyed, not orphaned"
		} else {
			ch.Note = "the slice is orphaned and its data kept; drop it from the instance's panel to reclaim the space"
		}
		return ch
	}
	switch {
	case tc.Type != "slice": // live slice, config says tile: replace
		p.Changes = append(p.Changes, deletion(),
			Change{Kind: "create", Env: env, Tile: name, New: tc.Type})
		return
	case ts.Slice == nil: // config slice over a live tile: replace
		if ts.Tile.ID != "" {
			p.Changes = append(p.Changes,
				Change{Kind: "delete", Env: env, Tile: name},
				Change{Kind: "create", Env: env, Tile: name, New: "slice"})
			return
		}
		p.Changes = append(p.Changes, Change{Kind: "create", Env: env, Tile: name, New: "slice",
			Note: "provisions the slice if it does not exist on the instance, adopts it if it does"})
		return
	}
	// instance identity compares the address's last segment only,
	// two visible instances sharing a slug across scopes would fool it; the
	// resolver refuses that ambiguity at apply.
	if FromInstance(tc.From) != ts.Slice.Instance || managedtiles.SliceName(ts.Slice.Engine, tc.SliceName(name)) != ts.Slice.Name {
		note := "moving a slice re-provisions it empty; the old data stays behind on " + ts.Slice.Instance
		ch := deletion()
		ch.Note = note
		p.Changes = append(p.Changes, ch,
			Change{Kind: "create", Env: env, Tile: name, New: "slice", Note: note})
		return
	}
	if removalPolicy(tc.OnRemove) != removalPolicy(ts.Slice.OnRemove) {
		p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
			Field: "on_remove", Old: removalPolicy(ts.Slice.OnRemove), New: removalPolicy(tc.OnRemove)})
	}
	if tc.Public != ts.Slice.Public {
		p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
			Field: "public", Old: strconv.FormatBool(ts.Slice.Public), New: strconv.FormatBool(tc.Public)})
	}
}

func (p *Plan) diffDomains(env, name string, tc TileConf, ts TileState, opts DiffOpts) {
	if tc.Type != "service" {
		return
	}
	cur := map[string]repo.Domain{}
	for _, d := range ts.Domains {
		cur[domainKey(d.Host, d.Path, d.Rule)] = d
	}
	seen := map[string]bool{}
	var wantHosts, haveHosts []string
	for _, dc := range tc.Domains {
		host, err := claimHost(dc, env, name, opts)
		if err != nil {
			p.Errors = append(p.Errors, fmt.Sprintf("env %s: %v", env, err))
			continue
		}
		key := domainKey(host, dc.Path, dc.Rule)
		seen[key] = true
		wantHosts = append(wantHosts, key)
		d, ok := cur[key]
		if !ok {
			if !p.claimHost(env, name, host+normPath(dc.Path), ts.Tile.ID) {
				continue
			}
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name, Field: "domain +" + host, New: domainLabel(dc, host)})
			continue
		}
		want := domainLabel(dc, host)
		have := domainLabelDB(d)
		if want != have {
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name, Field: "domain " + host, Old: have, New: want})
		}
		if port := domainPort(dc, tc, ts.Tile.ContainerPort, d.ContainerPort); port != d.ContainerPort {
			p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name, Field: "domain " + host + " port",
				Old: strconv.Itoa(d.ContainerPort), New: strconv.Itoa(port)})
		}
	}
	// ts.Domains comes position-ordered; a pure reorder changes the primary
	// (STACKR_PUBLIC_URL) without adding or removing anything.
	for _, d := range ts.Domains {
		haveHosts = append(haveHosts, domainKey(d.Host, d.Path, d.Rule))
	}
	if len(wantHosts) == len(haveHosts) && len(seen) == len(cur) {
		for i := range wantHosts {
			if wantHosts[i] != haveHosts[i] {
				p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name,
					Field: "domain order", Old: strings.Join(haveHosts, ", "), New: strings.Join(wantHosts, ", "),
					Note: "the first domain is what STACKR_PUBLIC_URL resolves to"})
				break
			}
		}
	}
	for _, key := range sortedKeys(cur) {
		d := cur[key]
		if seen[key] {
			continue
		}
		p.Changes = append(p.Changes, Change{Kind: "update", Env: env, Tile: name, Field: "domain -" + d.Host, Old: domainLabelDB(d)})
	}
}

// checkConnector refuses a connector: value this stack's org does not own.
//
// The value is a raw connector id taken straight from the file, and the
// connector behind it mints a GitHub installation token used to clone. Without
// this a stack file naming another org's id would borrow that org's credential
// and clone its private repositories. A plan error, not an apply one: by apply
// time the deploy is already running (docs/surface-parity.md, bugs found).
//
// An empty OrgConnectors means the lookup failed rather than "the org owns
// none", so it checks nothing: refusing every build over a database blip is
// worse than the deploy-time guard in infra/githubapp, which stands either way.
func (p *Plan) checkConnector(env, name string, tc TileConf, s State) {
	if tc.Connector == "" || len(s.OrgConnectors) == 0 || s.OrgConnectors[tc.Connector] {
		return
	}
	p.Errors = append(p.Errors, fmt.Sprintf("env %s: tile %s: connector %s belongs to another organisation", env, name, tc.Connector))
}

// claimHost records a tile's claim on host+path, or reports it taken when
// another tile (in the database or earlier in this plan) already routes it.
// Ownership ignores rule: a tile may route one host+path several ways, but a
// rule entry on another tile's host would take its traffic by priority.
func (p *Plan) claimHost(env, name, key, ownID string) bool {
	if owner, taken := p.hostOwner[key]; taken && owner != ownID && owner != "plan:"+env+"/"+name {
		p.Errors = append(p.Errors, fmt.Sprintf("env %s: tile %s: %s already routes to another service", env, name, key))
		return false
	}
	p.hostOwner[key] = "plan:" + env + "/" + name
	return true
}

// claimHosts runs the host checks for a tile the plan creates, the create
// path never reaches diffDomains, and a taken host there died on the unique
// index mid-apply.
func (p *Plan) claimHosts(env, name string, tc TileConf, ownID string, opts DiffOpts) {
	if tc.Type != "service" {
		return
	}
	for _, dc := range tc.Domains {
		host, err := claimHost(dc, env, name, opts)
		if err != nil {
			p.Errors = append(p.Errors, fmt.Sprintf("env %s: %v", env, err))
			continue
		}
		p.claimHost(env, name, host+normPath(dc.Path), ownID)
	}
}

// attachedSlug resolves a volume's attached tile id to its slug within the
// env snapshot ("" when detached or the target is gone).
func attachedSlug(cur EnvState, tileID string) string {
	if tileID == "" {
		return ""
	}
	for slug, ts := range cur.Tiles {
		if ts.Tile.ID == tileID {
			return slug
		}
	}
	return ""
}

func sortedKeys(m map[string]repo.Domain) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normPath collapses the two spellings of "no path". The web UI stores "/",
// config files omit it entirely, and Traefik treats them identically, but they
// key differently, so a config-declared domain looked missing and every plan
// wanted to re-add a row that was already there.
func normPath(p string) string {
	if p == "/" {
		return ""
	}
	return p
}

// domainKey is a domain row's identity, the unique index idx_domains_host_path:
// a rule entry can share its host and path with a plain one on the same tile.
// Ownership across tiles stays host+path (claimHost).
func domainKey(host, path, rule string) string {
	if rule != "" {
		return host + normPath(path) + " rule " + rule
	}
	return host + normPath(path)
}

// domainPort is the container port a declared domain routes to: its own
// port:, else the tile's. A row whose port was only tracking the tile's old
// port follows it; anything else set outside the file is left alone when the
// file names no port.
func domainPort(dc DomainConf, tc TileConf, oldTilePort, rowPort int) int {
	switch {
	case dc.Port != 0:
		return dc.Port
	case tc.Port != 0 && rowPort == oldTilePort:
		return tc.Port
	}
	return rowPort
}

// routeSuffix renders the routing keys both labels share.
func routeSuffix(mws []string, priority int, rule string) string {
	s := ""
	if rule != "" {
		s += " rule " + rule
	}
	if priority != 0 {
		s += " priority " + strconv.Itoa(priority)
	}
	if len(mws) > 0 {
		s += " via " + strings.Join(mws, ", ")
	}
	return s
}

func domainLabel(d DomainConf, host string) string {
	s := host + normPath(d.Path) + routeSuffix(d.Middlewares, d.Priority, d.Rule)
	if d.Auto {
		s += " (auto)"
	}
	if d.RedirectTo != "" {
		return s + " → " + d.RedirectTo
	}
	if !d.HTTPSOn() {
		s += " (http)"
	} else if !d.ForceHTTPSOn() {
		s += " (https, no redirect)"
	}
	return s
}

func domainLabelDB(d repo.Domain) string {
	s := d.Host + normPath(d.Path) + routeSuffix(d.MiddlewareList(), d.Priority, d.Rule)
	if d.Auto {
		s += " (auto)"
	}
	if d.RedirectTo != "" {
		return s + " → " + d.RedirectTo
	}
	if !d.HTTPS {
		s += " (http)"
	} else if !d.ForceHTTPS {
		s += " (https, no redirect)"
	}
	return s
}

func sourceLabel(t *repo.Tile) string {
	if t.SourceType == "image" {
		return "image " + t.ImageRef
	}
	return t.SourceType + " " + t.GitURL
}

func buildContext(tc TileConf) string {
	if tc.Build == nil {
		return ""
	}
	return tc.Build.Context
}

func dockerfile(tc TileConf) string {
	if tc.Build == nil {
		return ""
	}
	return tc.Build.Dockerfile
}

func normDot(s string) string {
	if s == "." {
		return ""
	}
	return s
}

// scopeRank orders sharing scopes from narrowest to widest, so a diff can tell
// "opening this up" from "cutting consumers off".
func scopeRank(s string) int {
	switch s {
	case "stack":
		return 1
	case "org":
		return 2
	default: // env
		return 0
	}
}

func defStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func defInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func joinNorm(lines []string) string {
	var out []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// canonEnv canonicalizes an env blob (sorted KEY=<quoted value>) so ordering
// and blank lines never show up as diffs. skip drops variables something else
// in the plan already accounts for (currently unused, nil).
//
// The value is quoted rather than written raw: the canonical form is joined
// with newlines, so a multi-line value used to be indistinguishable from two
// variables, and a tile carrying one (a PEM key, a JSON blob) never round
// tripped to an empty diff.
func canonEnv(raw string, skip map[string]bool) string {
	vars := envutil.Parse(raw)
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		if skip[v.Key] {
			continue
		}
		out = append(out, v.Key+"="+strconv.Quote(v.Value))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// envChangeSummary names which variables change without printing any value.
func envChangeSummary(oldCanon, newCanon string) string {
	parse := func(canon string) map[string]string {
		out := map[string]string{}
		for _, l := range strings.Split(canon, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				out[k] = v
			}
		}
		return out
	}
	before, after := parse(oldCanon), parse(newCanon)
	var added, removed, changed []string
	for k, v := range after {
		old, ok := before[k]
		switch {
		case !ok:
			added = append(added, k)
		case old != v:
			changed = append(changed, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	var parts []string
	for _, g := range []struct {
		label string
		names []string
	}{{"+", added}, {"~", changed}, {"-", removed}} {
		if len(g.names) == 0 {
			continue
		}
		sort.Strings(g.names)
		parts = append(parts, g.label+strings.Join(g.names, " "+g.label))
	}
	return strings.Join(parts, "  ")
}

func sortedTileNames(m map[string]TileConf) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStateNames(m map[string]TileState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEnvNames(m map[string]EnvState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Summary renders a one-line plan description ("3 to add, 1 to change, 2 to
// destroy", the terraform phrasing, because it works).
func (p *Plan) Summary() string {
	var add, change, destroy, set int
	for _, c := range p.Changes {
		if c.Declared() {
			// Counted apart: a generated secret is already reported as one,
			// and an unset value is somebody's to fill in, not a change.
			if c.Input {
				set++
			}
			continue
		}
		switch c.Kind {
		case "create", "create-env":
			add++
		case "delete", "delete-env":
			destroy++
		default:
			change++
		}
	}
	parts := []string{}
	if add > 0 {
		parts = append(parts, fmt.Sprintf("%d to add", add))
	}
	if change > 0 {
		parts = append(parts, fmt.Sprintf("%d to change", change))
	}
	if destroy > 0 {
		parts = append(parts, fmt.Sprintf("%d to destroy", destroy))
	}
	if set == 1 {
		parts = append(parts, "1 value to set")
	} else if set > 1 {
		parts = append(parts, fmt.Sprintf("%d values to set", set))
	}
	if n := len(p.GenSecrets); n == 1 {
		parts = append(parts, "1 secret to generate")
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d secrets to generate", n))
	}
	if len(parts) == 0 {
		parts = append(parts, "no changes")
	}
	if n := len(p.Errors); n == 1 {
		parts = append(parts, "1 error")
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d errors", n))
	}
	return strings.Join(parts, ", ")
}

// blockOnGroupMove records a pinned tile whose new node group its home node
// is not in.
//
// Changing a stateless tile's group just rolls it. Changing a pinned tile's
// group is a request to move its data to a different machine, and no config
// apply can do that: a local volume is a directory on one host's disk. The
// plan therefore stops and offers the Move, and apply stays blocked until
// every one is done (docs/plans/31-node-agent-open-questions.md, step 8).
//
// Only the group's own change is checked here. Whether the home node actually
// carries the label is settled at deploy time by infra/placement, which reads
// the live node, the plan has no swarm connection and must not grow one.
func (p *Plan) blockOnGroupMove(env, name string, t repo.Tile, want string) {
	if want == "" || want == t.NodeGroup || t.HomeNode == "" {
		return
	}
	p.Moves = append(p.Moves, MoveBlock{
		TileID: t.ID, Tile: name, Env: env,
		FromNode: t.HomeNode, ToGroup: want,
	})
}
