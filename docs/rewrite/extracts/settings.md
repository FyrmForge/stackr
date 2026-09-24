# config/settings

Source: `config/settings/` (keys.go, settings.go, settings_test.go, cascade_test.go)
Commit: c2423f0
Taken: the two key lists folded into one catalogue, the cascade (server → org → stack → env → tile), `Resolve`, the tile rung (`Effective*`), `Check`, and `Merge`'s field semantics with B24 fixed (a typo is refused, never read as "not set").
Cut: the form binder (`url.Values`, `field`/`str`/`num`/`numF`, the hidden-checkbox idiom), every store-walking helper (`ForServer`/`ForOrg`/`ForStack`/`ForTile`/`TryTile`/`Levels`/`Chain`/`TryChain`), the `repo.Store` import.
Cuts belong to: the binder to each surface (`leaf/cli`, the web handler); the chain walk to `SettingsService`, which loads the four rows and hands the values to `Resolve`.

## One catalogue

The old package held two unrelated key spaces that both called themselves
settings: the typed `settings` **column** on server/org/stack/env rows (the
cascade), and the flat `settings` **table** behind `GetSetting`/`SetSetting`
(keys.go, no levels, no schema). keys.go exists at all because those flat keys
had been typed by hand in five packages, and a key typed a second way reads
back `""` — the knob is silently off. `dns_provider` was read by two packages
with nothing saying they meant the same thing.

The plan's "one catalogue every surface enumerates" is the fix for both: one
entry per knob, and the entry is the only place its name is spelled.

```go
// leaf/settings/catalogue.go
type Type string

const (
	TInt   Type = "int"
	TFloat Type = "float"
	TBool  Type = "bool"
	TStr   Type = "string"
)

// Scope is which rungs of the cascade may set a knob. Old code left this in
// comments ("instance-wide and only read at the server level"), so a stack
// could store build_node and nothing ever read it back.
type Scope uint8

const (
	Server Scope = 1 << iota
	Org
	Stack
	Env
	Tile

	Below = Org | Stack | Env
	All   = Server | Below
)

// String names a single scope, so a refusal reads "build_node cannot be set on
// a stack" rather than printing the bitmask.
func (s Scope) String() string { ... }

// Knob is one catalogue entry. Key is the ONLY spelling of the name: JSON tag,
// CLI flag, form field, flat-table row, API field.
type Knob struct {
	Key       string
	Type      Type
	Default   string // rendered form of the builtin, for `settings list`
	Scopes    Scope
	AllowZero bool // an explicit 0 is a real value ("unlimited"), not "clear"
	Later     bool // catalogued so the name is reserved; not wired in v1
	Desc      string
}

// Catalogue is enumerated, never indexed by a literal at a call site.
var Catalogue = []Knob{
	{Key: "cpu_limit", Type: TFloat, Default: "0", Scopes: All | Tile, AllowZero: true,
		Desc: "CPU cores; 0 is unlimited"},
	{Key: "mem_limit_mb", Type: TInt, Default: "0", Scopes: All | Tile, AllowZero: true,
		Desc: "memory cap in MB; 0 is unlimited"},
	{Key: "protect", Type: TBool, Default: "false", Scopes: All,
		Desc: "basic auth in front of every URL below this level"},
	{Key: "protect_user", Type: TStr, Scopes: All, Desc: "basic auth user"},
	{Key: "protect_password", Type: TStr, Scopes: All, Desc: "plain text or a ${{ }} ref"},
	// ... the rest of the table below, one entry each.
}
```

## The cascade

```go
// leaf/settings/settings.go
// Settings is one level's overrides. A nil field is "inherit"; a pointer is
// what makes an explicit 0 survive both the cascade and the JSON round trip
// (TestExplicitZeroSurvivesJSONRoundTrip).
type Settings struct {
	CPULimit        *float64 `json:"cpu_limit,omitempty"`
	MemLimitMB      *int     `json:"mem_limit_mb,omitempty"`
	Protect         *bool    `json:"protect,omitempty"`
	ProtectUser     *string  `json:"protect_user,omitempty"`
	ProtectPassword *string  `json:"protect_password,omitempty"`
	// ... one field per catalogue entry.
}

// Resolved is the fully-cascaded result, every field concrete.
type Resolved struct {
	CPULimit        float64
	MemLimitMB      int
	Protect         bool
	ProtectUser     string
	ProtectPassword string
}

var builtin = Resolved{ /* the Default column, typed */ }

// Parse decodes a stored blob; bad or empty input is "no overrides".
func Parse(blob string) Settings {
	var s Settings
	_ = json.Unmarshal([]byte(blob), &s)
	return s
}

func (s Settings) JSON() string { b, _ := json.Marshal(s); return string(b) }

// Resolve applies the levels in order (server, org, stack, env); later wins
// where set, and unset falls through to the builtin.
//
// extract: values now passed in. The old package reached into repo.Store here
// (ForOrg/ForStack/ForTile/Chain), which is why every caller that wanted a
// number also carried a store handle. SettingsService loads the four rows,
// Parses each, and calls this.
func Resolve(levels ...Settings) Resolved {
	r := builtin
	for _, s := range levels {
		if s.CPULimit != nil {
			r.CPULimit = *s.CPULimit
		}
		if s.MemLimitMB != nil {
			r.MemLimitMB = *s.MemLimitMB
		}
		if s.Protect != nil {
			r.Protect = *s.Protect
		}
		// One unit: a level that sets only the user must not pair it with a
		// password from the level above, or nobody can log in
		// (TestProtectCredentialsResolveAsOneUnit).
		if s.ProtectUser != nil || s.ProtectPassword != nil {
			r.ProtectUser, r.ProtectPassword = deref(s.ProtectUser), deref(s.ProtectPassword)
		}
		// ... one block per field.
	}
	return r
}
```

The **tile is the fifth rung**, and it is not a `Settings` — a tile carries its
own columns and folds them in at read time. Zero means inherit:

```go
// EffectiveLimits folds a tile's own limits over the resolved ones.
func (r Resolved) EffectiveLimits(cpu float64, memMB int) (float64, int) {
	if cpu == 0 {
		cpu = r.CPULimit
	}
	if memMB == 0 {
		memMB = r.MemLimitMB
	}
	return cpu, memMB
}
// extract: dropped EffectiveGroup (node_group) and EffectiveTimeout (cron),
// belongs with the cron and multi-node rows when they land; same three-line
// shape, and the cron one inherits on <=0 rather than on ==0.
```

## Provenance, store-free

```go
// Level is one rung, named so a settings page can say where an inherited value
// came from instead of always blaming the server.
type Level struct {
	Scope    Scope
	Name     string // "this server", the org/stack/env name
	Settings Settings
}

// Above resolves the levels over the one given, which is the "inherited: 512
// MB (from org Acme)" line on a settings page.
func Above(levels []Level, at Scope) Resolved {
	var chain []Settings
	for _, l := range levels {
		if l.Scope == at {
			break
		}
		chain = append(chain, l.Settings)
	}
	return Resolve(chain...)
}
// extract: dropped Levels/Chain/TryChain, belongs in SettingsService — they
// walked env→stack→org in the store to build this slice. The old split between
// Chain (swallows the store error) and TryChain (keeps it) existed because a
// failed load looks exactly like a level that sets nothing, and for Protect
// that reads as "not protected" and publishes the URL. With the walk in the
// service there is one function and it returns the error.
```

## Writing a level: B24

```go
// Set applies one submitted value to one field. The only string → field door;
// every surface goes through it, so a knob is parsed one way everywhere.
//
// B24: a value that does not parse is REFUSED. The old num/numF set the field
// to nil on a parse error, so "51 2" on the memory field silently cleared the
// override and the tile inherited a cap the operator thought they had just
// raised (settings_test.go "garbage clears rather than corrupting" asserted
// the bug). Empty string still clears to inherit — that is a different input.
func Set(s *Settings, key, raw string) error {
	k, ok := lookup(key)
	if !ok {
		return fmt.Errorf("no setting named %q", key)
	}
	raw = strings.TrimSpace(raw)
	switch key {
	case "mem_limit_mb":
		return setInt(&s.MemLimitMB, k, raw)
	case "cpu_limit":
		return setFloat(&s.CPULimit, k, raw)
	// ... one case per catalogue entry.
	}
}

func setInt(dst **int, k Knob, raw string) error {
	if raw == "" {
		*dst = nil // inherit from the level above
		return nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("%s: %q is not a whole number", k.Key, raw)
	}
	if v < 0 || (v == 0 && !k.AllowZero) {
		return fmt.Errorf("%s: %q is not a usable value", k.Key, raw)
	}
	*dst = &v
	return nil
}

// Merge folds a set of submitted values into an existing level. A key that was
// not submitted is left alone: a form, a CLI call and a PATCH all show a subset
// of the catalogue, and a partial save must not wipe the rest
// (TestMergeKeepsUnsubmittedFields). Presence of the key is the submission;
// its emptiness is the clear.
//
// All-or-nothing: the first bad value refuses the whole save, so a rejected
// form never half-applies.
func Merge(s Settings, vals map[string]string, at Scope) (Settings, error) {
	for _, k := range Catalogue { // catalogue order, so errors are stable
		raw, ok := vals[k.Key]
		if !ok {
			continue
		}
		if k.Scopes&at == 0 {
			return s, fmt.Errorf("%s cannot be set on a %s", k.Key, at)
		}
		if err := Set(&s, k.Key, raw); err != nil {
			return s, err
		}
	}
	return s, s.Check()
}
// extract: dropped the url.Values plumbing (field/str/num/numF) and the
// last-value-wins rule behind it, belongs in the web binder — an unticked
// checkbox submits nothing, so the form pairs it with a hidden "0" under the
// same name and the binder collapses that to one map entry before calling here.
// extract: dropped "a blank password means leave it alone", belongs in the web
// binder too: it existed because the form cannot render a stored password back
// into the input. The rule here is the honest one — a surface that cannot show
// a value does not submit its key.

// Check refuses a level that sets half of the basic auth pair. Resolve takes
// user and password as one unit, so a level with only one of them resolves the
// other to empty and every URL below is locked behind a password nobody knows.
// Caught at the save, where the operator can still see it.
// It takes no scope: which level may set which knob is Merge's job, above.
func (s Settings) Check() error {
	user := s.ProtectUser != nil && *s.ProtectUser != ""
	pass := s.ProtectPassword != nil && *s.ProtectPassword != ""
	switch {
	case user == pass:
		return nil
	case user:
		return errors.New("protection needs a password as well as a user")
	default:
		return errors.New("protection needs a user as well as a password")
	}
}
// Protection off does not excuse half a pair, and both unset is fine —
// credentials inherit (TestCheckProtectPair).
```

## The knobs

Scope `server` means instance-wide, read only off the server row. `flat` is the
level-less k/v table (old `keys.go`), which the catalogue swallows.

| key | type | default | scope | v1/later |
|---|---|---|---|---|
| `cpu_limit` | float | `0` (unlimited) | server→env, tile | v1 |
| `mem_limit_mb` | int MB | `0` (unlimited) | server→env, tile | v1 |
| `protect` | bool | `false` | server→env | v1 |
| `protect_user` | string | `""` | server→env (pair) | v1 |
| `protect_password` | string | `""` | server→env (pair) | v1 |
| `backup_run_concurrency` | int | `2` | server | v1 |
| `backup_restore_concurrency` | int | `1` | server | v1 |
| `cron_timeout_min` | int min | `30` | server→env, tile | later — cron tiles |
| `cron_run_concurrency` | int | `8` | server | later — cron tiles |
| `run_retention_days` | int days | `30` | server→env | later — cron runs |
| `metric_retention_hours` | int h | `24` | server→env | later — metrics |
| `node_group` | string | `""` (anywhere) | server→env, tile | later — multi-node |
| `build_node` | string | `""` (the manager) | server | later — Swarm |
| `volume_move_concurrency` | int | `2` | server | later — volume moves |
| `proxy_custom_dynamic` | string | `""` | flat | v1 — becomes the admin-only extra Caddy config |
| `traefik_static_override` | string | `""` | flat | cut — Traefik-only, and REWRITE.md leaves it; Caddy has one config and the row above is it. Listed so the name is recognised, not reserved |
| `trusted_proxies` | CIDR list | `""` | flat | v1 |
| `trust_cloudflare` | bool | off | flat | v1 |
| `cloudflare_cidrs` | CIDR list | `""` | flat | v1 — cached sweep of Cloudflare's published ranges |
| `dns_provider` | string | `""` | flat | v1 — DNS-01, needed for the `*.<root>` certificate |
| `dns_env` | k=v creds | `""` | flat | v1 |
| `upgrade_latest` | version | `""` | flat | v1 |
| `upgrade_checked_at` | RFC3339 | `""` | flat | v1 |
| `upgrade_archive` | string | `""` | flat | v1 |
| `image_check_interval` | int min | `""`→5, `"0"`→off | flat | v1 — the janitor ticks every minute, which is what makes the cadence runtime-changeable |
| `image_check_last` | RFC3339 | `""` | flat | v1 |
| `cleanup_enabled` | bool | off | flat | v1 — the nightly docker/registry sweep |
| `install_seeded` | bool | `""` | flat | v1 — set once; after first boot the panel owns the installer's answers, and a value the operator removed must not come back on the next restart |
| `prplan.<stack>.<pr>` | markdown | `""` | flat | later — PR comments |

## Notes for the builder

- The proxy escape hatches live in the DB rather than on disk **so a wiped data
  dir heals from the next resync**. Keep that, whatever the Caddy config sink
  looks like.
- The two key spaces stay distinguishable inside the one catalogue: a `flat`
  knob has no levels, so `Resolve` never sees it and `Scope` is what says so.
  If that turns into two catalogues again, the five-packages-one-typo bug comes
  back with it.
- The catalogue and the `Set` switch are two lists that must agree. One test
  keeps them honest: for every `Catalogue` entry, `Set` accepts a valid value
  and the field comes back non-nil; and by reflection, the count of entries
  that are neither `Later` nor `flat`-scoped equals the number of `Settings`
  fields, so a new field cannot skip the table.
- Second test, for B24: `Merge` with `mem_limit_mb: "51 2"` returns an error
  **and** leaves the existing override in place.
- B24 is wider than that one input. `setInt`/`setFloat` refuse all three of
  parse failure, negative, and zero where `AllowZero` is false; the old helpers
  cleared the field on all three, and three old tests enshrine that — "garbage
  clears rather than corrupting", "negative clears", and "zero timeout clears
  instead (no meaningful zero)". All three invert. The third changes how a
  surface clears `cron_timeout_min`: it submits the key empty, not `0`.
- `Scope` on a knob is new enforcement, not a port. The old code only wrote
  "instance-wide, only read at the server level" in a comment, so a stack could
  store `build_node` and nothing read it. Cheap here, and it is what stops the
  `later` rows from being set on levels that will never grow them.
- Explicit zero is the whole reason the fields are pointers: `cpu_limit: 0` at a
  lower level is how a tile says "unlimited" once a parent sets a cap. Nothing
  about this may go through a value field with `omitempty`.
- `later` knobs stay in the catalogue but out of `Settings`/`Resolved` until
  their feature lands — the name is reserved, not implemented. A surface
  enumerating the catalogue skips `Later` entries.
- Failure to load a level is not the same as a level that sets nothing. Whatever
  in `SettingsService` walks the rows returns its error; the old `Chain`
  swallowed it, and for `Protect` that resolves to "not protected" and publishes
  a URL that should have been behind basic auth.

Size: source 805 lines, extract 365 lines
