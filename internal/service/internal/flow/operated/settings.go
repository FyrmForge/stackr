package operated

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// LiveSetting is one setting as the running instance reports it.
type LiveSetting struct {
	Value   string
	Pending bool // a restart would change it
}

// pgConfWrite is the argv that replaces stackr.conf atomically (temp file,
// then rename) and hands it to postgres.
func pgConfWrite(i Instance, conf string) []string { return pgFileWrite(i, "stackr.conf", conf) }

// pgFileWrite is the argv that replaces file name of the data directory
// atomically.
func pgFileWrite(i Instance, name, content string) []string {
	script := `d="$0"; printf '%s' "$2" > "$d/$1.new" && chown postgres:postgres "$d/$1.new" && mv "$d/$1.new" "$d/$1"`
	return []string{"sh", "-c", script, pgDataDir(i.Major), name, content}
}

// postgres is the instance's engine, refused for any other.
func (f *Flow) postgres(ctx context.Context, it store.Tile) (store.OperatedInstance, Instance, Tools, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return m, Instance{}, Tools{}, err
	}
	if _, ok := e.(pg); !ok {
		return m, Instance{}, Tools{}, errs.Invalidf("engine", "%s has no settings; only postgres does", m.Engine)
	}
	return m, f.facts(ctx, it, m, e.Definition()), f.tools(ctx, it, m), nil
}

// LiveSettings reads names from the running instance, and whether any
// setting at all waits for a restart. A name the instance does not know is
// absent from the map.
func (f *Flow) LiveSettings(ctx context.Context, it store.Tile, names []string) (map[string]LiveSetting, bool, error) {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return nil, false, err
	}
	return pgLive(ctx, i, x, names)
}

func pgLive(ctx context.Context, i Instance, x Tools, names []string) (map[string]LiveSetting, bool, error) {
	psql := pg{}.psql(i, i.AdminDB, nil)
	q := `SELECT name || '|' || pending_restart::int || '|' || current_setting(name) || '|' || context ` +
		`FROM pg_settings WHERE name IN (` + pgInList(names) + `)`
	out, err := x.Exec(ctx, append(psql, "-tA", "-c", q))
	if err != nil {
		return nil, false, err
	}
	live := map[string]LiveSetting{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.SplitN(l, "|", 4)
		if len(p) < 4 {
			continue
		}
		live[p[0]] = LiveSetting{Value: p[2], Pending: p[1] == "1"}
	}
	pending, err := x.Exec(ctx, append(psql, "-tA", "-c", `SELECT coalesce(bool_or(pending_restart), false)::int FROM pg_settings`))
	return live, strings.TrimSpace(pending) == "1", err
}

// ApplySettings makes the running instance's stackr.conf the effective
// settings for the user's overrides, and reports whether the change needs a
// restart (a changed setting only takes effect at start) rather than a
// reload, which it has already done when not. changed are the names whose
// override was set or removed (unset the removed ones). Names the instance
// does not know and values it rejects are refused with the file as it was.
// The caller redeploys on restart: the new container writes the same conf
// from its env.
func (f *Flow) ApplySettings(
	ctx context.Context,
	it store.Tile,
	overrides map[string]string,
	changed, unset []string,
) (restart bool, err error) {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return false, err
	}
	i = f.withNodeMem(ctx, it, i)
	i.Settings = overrides
	return pgApply(ctx, i, x, changed, unset)
}

// pgApply is ApplySettings on the instance's facts and tools.
func pgApply(ctx context.Context, i Instance, x Tools, changed, unset []string) (restart bool, err error) {
	ctxs, err := pgContexts(ctx, i, x, changed)
	if err != nil {
		return false, err
	}
	if restart, err = pgJudge(ctxs, changed, unset); err != nil {
		return false, errs.Invalidf("settings", "%s", err.Error())
	}
	old, err := x.Exec(ctx, []string{"sh", "-c", `cat "$0" 2>/dev/null; true`, pgDataDir(i.Major) + "/stackr.conf"})
	if err != nil {
		return false, err
	}
	if _, err := x.Exec(ctx, pgConfWrite(i, pgConf(i))); err != nil {
		return false, err
	}
	// from here a failure puts the old file back and reloads it
	undo := func() {
		_, _ = x.Exec(ctx, pgConfWrite(i, old))
		_, _ = x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_reload_conf()`))
	}
	// pg_file_settings parses the file as a reload would; a postmaster
	// setting waiting for a restart is not an error.
	bad, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c",
		`SELECT string_agg(name || ': ' || error, '; ') FROM pg_file_settings `+
			`WHERE sourcefile LIKE '%/stackr.conf' AND error IS NOT NULL AND error <> 'setting could not be applied'`))
	if err == nil && strings.TrimSpace(bad) != "" {
		undo()
		return false, errs.Invalidf("settings", "postgres refuses it: %s", strings.TrimSpace(bad))
	}
	if err != nil {
		undo()
		return false, err
	}
	if !restart {
		if _, err = x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_reload_conf()`)); err != nil {
			undo()
			return false, err
		}
	}
	return restart, nil
}

// pgContexts is pg_settings.context of each name the instance knows.
func pgContexts(ctx context.Context, i Instance, x Tools, names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c",
		`SELECT name || '|' || context FROM pg_settings WHERE name IN (`+pgInList(names)+`)`))
	if err != nil {
		return nil, err
	}
	c := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if n, ctxt, ok := strings.Cut(l, "|"); ok {
			c[n] = ctxt
		}
	}
	return c, nil
}

// ChangedSettings are the names whose override differs between old and cur
// (set, changed or removed), sorted, and the removed ones.
func ChangedSettings(old, cur map[string]string) (changed, unset []string) {
	for k, v := range cur {
		if o, ok := old[k]; !ok || o != v {
			changed = append(changed, k)
		}
	}
	for k := range old {
		if _, ok := cur[k]; !ok {
			changed = append(changed, k)
			unset = append(unset, k)
		}
	}
	slices.Sort(changed)
	slices.Sort(unset)
	return changed, unset
}

// SettingsPlanError is a stack file's refusal of its settings, before any
// instance exists to ask.
func SettingsPlanError(engine string, s map[string]string) error {
	if len(s) == 0 {
		return nil
	}
	if engine != "postgres" {
		return fmt.Errorf("%s has no settings; only postgres does", engine)
	}
	return CheckSettings(s)
}
