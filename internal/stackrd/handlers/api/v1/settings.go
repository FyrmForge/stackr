package v1

// The defaults cascade: server -> organization -> stack -> environment.
//
// Each level shows what it inherits, where that came from, and its own
// override. Reading the resolved value alone would tell a caller what applies
// and nothing about which level to change to move it.

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// settingsTarget is one level and everything above it, which is what both the
// read and the write need.
type settingsTarget struct {
	kind    string // server | org | stack | env
	org     *repo.Org
	stack   *repo.Stack
	env     *repo.Environment
	server  *repo.Server
	current settings.Settings
}

// resolveSettingsTarget reads the level the route addresses, checking access
// the same way every other route on that object does.
func (a *API) resolveSettingsTarget(c echo.Context, kind string, write bool) (*settingsTarget, error) {
	ctx := c.Request().Context()
	t := &settingsTarget{kind: kind}
	switch kind {
	case "server":
		if !a.isAdmin(c) {
			return nil, echo.NewHTTPError(http.StatusForbidden, "server defaults are admin-only")
		}
		sv, err := a.store.GetServer(ctx, "local")
		if err != nil || sv == nil {
			return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		t.server, t.current = sv, settings.Parse(sv.Settings)
	case "org":
		o, err := a.requireOrg(c, c.Param("id"))
		if err != nil {
			return nil, err
		}
		if write {
			if err := a.requireOrgWrite(ctx, c, o.ID); err != nil {
				return nil, err
			}
		}
		t.org, t.current = o, settings.Parse(o.Settings)
	case "stack":
		s, err := a.requireStackAccess(c, c.Param("id"))
		if err != nil {
			return nil, err
		}
		if write {
			if err := a.requireOrgWrite(ctx, c, s.OrgID); err != nil {
				return nil, err
			}
		}
		t.stack, t.current = s, settings.Parse(s.Settings)
	default:
		env, _, err := a.requireEnvWrite(c, c.Param("id"))
		if !write {
			env, err = a.requireEnvAccess(c, c.Param("id"))
		}
		if err != nil {
			return nil, err
		}
		t.env, t.current = env, settings.Parse(env.Settings)
	}
	return t, nil
}

// levels returns the chain above and including this target, so a reader can
// see which level a value actually comes from.
func (a *API) settingsLevels(c echo.Context, t *settingsTarget) []settings.Level {
	ctx := c.Request().Context()
	switch t.kind {
	case "server":
		return settings.Levels(ctx, a.store, "", "", "")
	case "org":
		return settings.Levels(ctx, a.store, t.org.ID, "", "")
	case "stack":
		return settings.Levels(ctx, a.store, "", t.stack.ID, "")
	}
	return settings.Levels(ctx, a.store, "", "", t.env.ID)
}

func (a *API) settingsFor(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		t, err := a.resolveSettingsTarget(c, kind, false)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, a.toSettingsOut(c, t))
	}
}

// patchSettingsFor writes this level's own overrides. Only the keys present in
// the body are touched; a key sent empty clears the override back to inherit,
// which is how a level gives a value back to the one above it.
func (a *API) patchSettingsFor(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		t, err := a.resolveSettingsTarget(c, kind, true)
		if err != nil {
			return err
		}
		var in map[string]*string
		if err := c.Bind(&in); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
		}
		vals := url.Values{}
		for k, v := range in {
			if v == nil {
				// JSON null is the explicit "give it back to the level above";
				// settings.Merge reads an unparseable value as a clear.
				vals.Set(k, "")
				continue
			}
			vals.Set(k, *v)
		}
		merged := settings.Merge(t.current, vals).JSON()
		ctx := c.Request().Context()
		switch t.kind {
		case "server":
			t.server.Settings = merged
			err = a.store.UpdateServer(ctx, t.server)
		case "org":
			t.org.Settings = merged
			err = a.store.UpdateOrg(ctx, t.org)
		case "stack":
			t.stack.Settings = merged
			err = a.store.UpdateStack(ctx, t.stack)
		default:
			t.env.Settings = merged
			err = a.store.UpdateEnvironment(ctx, t.env)
		}
		if err != nil {
			return err
		}
		t.current = settings.Parse(merged)
		return c.JSON(http.StatusOK, a.toSettingsOut(c, t))
	}
}

// toSettingsOut renders one row per knob: the resolved value, which level it
// came from, and this level's own override if it has one.
func (a *API) toSettingsOut(c echo.Context, t *settingsTarget) settingsOut {
	levels := a.settingsLevels(c, t)
	res := settings.Resolve(chainOf(levels)...)
	out := settingsOut{Level: t.kind, Values: []settingKnob{}}
	add := func(key, value string, own *string, source string) {
		out.Values = append(out.Values, settingKnob{Key: key, Value: value, Source: source, Own: own})
	}
	for _, k := range settingKeys {
		own, source := ownAndSource(levels, t.kind, k.key)
		add(k.key, k.resolved(res), own, source)
	}
	return out
}

func chainOf(levels []settings.Level) []settings.Settings {
	out := make([]settings.Settings, 0, len(levels))
	for _, l := range levels {
		out = append(out, l.Settings)
	}
	return out
}

// settingKeys is the knob catalogue: the wire name, and how to read the
// resolved value and one level's own override for it. One list, so a new knob
// cannot appear on one surface and not the other.
var settingKeys = []struct {
	key      string
	resolved func(settings.Resolved) string
	own      func(settings.Settings) *string
}{
	{"cron_timeout_min",
		func(r settings.Resolved) string { return strconv.Itoa(r.CronTimeoutMin) },
		func(s settings.Settings) *string { return intStr(s.CronTimeoutMin) }},
	{"cpu_limit",
		func(r settings.Resolved) string { return strconv.FormatFloat(r.CPULimit, 'f', -1, 64) },
		func(s settings.Settings) *string { return floatStr(s.CPULimit) }},
	{"mem_limit_mb",
		func(r settings.Resolved) string { return strconv.Itoa(r.MemLimitMB) },
		func(s settings.Settings) *string { return intStr(s.MemLimitMB) }},
	{"run_retention_days",
		func(r settings.Resolved) string { return strconv.Itoa(r.RunRetentionDays) },
		func(s settings.Settings) *string { return intStr(s.RunRetentionDays) }},
	{"metric_retention_hours",
		func(r settings.Resolved) string { return strconv.Itoa(r.MetricRetentionHours) },
		func(s settings.Settings) *string { return intStr(s.MetricRetentionHours) }},
	{"protect_auto_domains",
		func(r settings.Resolved) string { return boolStr(r.ProtectAutoDomains) },
		func(s settings.Settings) *string {
			if s.ProtectAutoDomains == nil {
				return nil
			}
			v := boolStr(*s.ProtectAutoDomains)
			return &v
		}},
	{"node_group",
		func(r settings.Resolved) string { return r.NodeGroup },
		func(s settings.Settings) *string { return s.NodeGroup }},
	{"build_node",
		func(r settings.Resolved) string { return r.BuildNode },
		func(s settings.Settings) *string { return s.BuildNode }},
}

// ownAndSource finds this level's own override for a key and names the deepest
// level that sets it, which is what a reader needs to know where to go and
// change it.
func ownAndSource(levels []settings.Level, kind, key string) (own *string, source string) {
	var read func(settings.Settings) *string
	for _, k := range settingKeys {
		if k.key == key {
			read = k.own
			break
		}
	}
	if read == nil {
		return nil, ""
	}
	source = "built-in"
	for _, l := range levels {
		v := read(l.Settings)
		if v == nil {
			continue
		}
		source = l.Kind
		if l.Kind == kind {
			own = v
		}
	}
	return own, source
}

func intStr(p *int) *string {
	if p == nil {
		return nil
	}
	v := strconv.Itoa(*p)
	return &v
}

func floatStr(p *float64) *string {
	if p == nil {
		return nil
	}
	v := strconv.FormatFloat(*p, 'f', -1, 64)
	return &v
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
