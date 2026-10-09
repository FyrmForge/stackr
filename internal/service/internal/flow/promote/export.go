package promote

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// omap is a mapping that keeps the order keys were put in, so the file reads
// top down (version, stack, params ...) and a zero value is simply never put.
type omap []kv

type kv struct {
	k string
	v any
}

func (m *omap) put(k string, v any) { *m = append(*m, kv{k, v}) }

func (m omap) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, e := range m {
		var k, v yaml.Node
		if err := k.Encode(e.k); err != nil {
			return nil, err
		}
		if err := v.Encode(e.v); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &k, &v)
	}
	return n, nil
}

// Export writes a stack live as a stackr-compose.yml: every env of the
// ladder (envID ""), or just the one env, with the tiles, volumes, params and
// domains as the file keys that make them, defaults left out. A promote env
// carries from: promote. A secret is its name only, a ref stays a ref.
// Planning the result against each exported env reads clean. Read-only.
//
// ponytail: the file has one params: block for the whole stack, so a param
// that differs between envs (or is missing in some) keeps its name and loses
// its value, with a warning. Backup schedules, literal domain hosts that came
// from a params ref, a literal basic-auth password and a literal protect
// password are not carried. A promote env exported alone has no rung below it
// in the file, so its from: is left out.
func (f *Flow) Export(ctx context.Context, stackID, envID string) ([]byte, []string, error) {
	var warns []string
	d := f.D
	st, err := d.Stacks.Get(ctx, stackID)
	if err != nil {
		return nil, nil, err
	}
	ladder, err := d.Envs.Ladder(ctx, st.ID)
	if err != nil {
		return nil, nil, err
	}
	var envs []store.Environment
	for i, e := range ladder {
		if envID != "" && e.ID != envID {
			continue
		}
		if e.FromKind == environment.FromPromote && (i == 0 || envID != "") {
			warns = append(warns, "environment "+e.Slug+": promotes from a rung the file does not hold; from: left out")
		}
		envs = append(envs, e)
	}
	if len(envs) == 0 {
		return nil, nil, errs.Conflictf("This stack has no environment to export.")
	}
	res, err := f.Resources.ListAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out omap
	out.put("version", 1)
	out.put("stack", st.Slug)

	ps, err := f.exportParams(ctx, st, envs, &warns)
	if err != nil {
		return nil, nil, err
	}
	if len(ps) > 0 {
		out.put("params", ps)
	}
	if m := defaultsMap(st.Settings); len(m) > 0 {
		out.put("defaults", m)
	}
	var doms []any
	for _, r := range res {
		if r.StackID == nil || *r.StackID != st.ID {
			continue
		}
		x := omap{{"host", r.Host}}
		if r.ACMEEmail != "" {
			x.put("acme_email", r.ACMEEmail)
		}
		if r.IncludeEnvOnDefault {
			x.put("include_env_on_default", true)
		}
		doms = append(doms, x)
	}
	if len(doms) > 0 {
		out.put("domains", doms)
	}

	em := omap{}
	for _, e := range envs {
		body, err := f.exportEnv(ctx, st, e, ladder[0].ID == e.ID || envID != "", res, &warns)
		if err != nil {
			return nil, nil, err
		}
		em.put(e.Slug, body)
	}
	out.put("environments", em)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), warns, enc.Close()
}

// exportParams is the file's params: block. A name is declared once for the
// whole stack: a stack-scope entry wins, an env-scope one carries its value
// only when every exported env holds the same.
func (f *Flow) exportParams(ctx context.Context, st store.Stack, envs []store.Environment, warns *[]string) (map[string]map[string]any, error) {
	stackVals, err := f.D.Params.Values(ctx, params.Scope{Kind: "stack", ID: st.ID}, true)
	if err != nil {
		return nil, err
	}
	envVals := make([]map[string]params.Value, len(envs))
	keys := map[string]bool{}
	for k := range stackVals {
		keys[k] = true
	}
	for i, e := range envs {
		if envVals[i], err = f.D.Params.Values(ctx, params.Scope{Kind: "env", ID: e.ID}, true); err != nil {
			return nil, err
		}
		for k := range envVals[i] {
			keys[k] = true
		}
	}
	out := map[string]map[string]any{}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		d, ok := declare(key, stackVals, envVals, warns)
		if !ok {
			continue
		}
		c, n, _ := strings.Cut(key, ".")
		if out[c] == nil {
			out[c] = map[string]any{}
		}
		out[c][n] = d
	}
	return out, nil
}

func declare(key string, stack map[string]params.Value, envs []map[string]params.Value, warns *[]string) (omap, bool) {
	if v, ok := stack[key]; ok {
		for _, ev := range envs {
			if e, ok := ev[key]; ok && (e.Secret != v.Secret || e.V != v.V) {
				*warns = append(*warns, "params."+key+": an environment overrides the stack value; the override is not carried")
				break
			}
		}
		return declOf(v), true
	}
	var first params.Value
	same, n := true, 0
	for _, ev := range envs {
		v, ok := ev[key]
		if !ok {
			continue
		}
		switch {
		case n == 0:
			first = v
		case v.Secret != first.Secret:
			*warns = append(*warns, "params."+key+": a secret in one environment and a param in another; left out")
			return nil, false
		case v.V != first.V:
			same = false
		}
		n++
	}
	if !first.Secret && (!same || n < len(envs)) {
		*warns = append(*warns, "params."+key+": not the same in every environment; declared without a value")
		return omap{{"type", params.Param}}, true
	}
	return declOf(first), true
}

func declOf(v params.Value) omap {
	if v.Secret {
		return omap{{"type", params.Secret}}
	}
	return omap{{"type", params.Param}, {"value", v.V}}
}

// exportEnv is one environment section: its from/branch, knobs, tiles and
// volumes. alone is true when the env has no rung below it in the file.
func (f *Flow) exportEnv(ctx context.Context, st store.Stack, e store.Environment, alone bool, res []store.DomainResource, warns *[]string) (omap, error) {
	d := f.D
	env := omap{}
	switch {
	case e.FromKind == environment.FromBranch:
		env.put("branch", e.FromBranch)
		if !e.Auto {
			env.put("auto", false)
		}
	case e.FromKind == environment.FromPromote && !alone:
		env.put("from", environment.FromPromote)
		if e.Auto {
			env.put("auto", true)
		}
	}
	if e.Color != "" {
		env.put("color", e.Color)
	}
	if m := defaultsMap(e.Settings); len(m) > 0 {
		env.put("defaults", m)
	}
	live, err := d.Tiles.List(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(live, func(a, b store.Tile) int { return cmp.Compare(a.Slug, b.Slug) })
	tiles := omap{}
	for _, t := range live {
		body, err := f.exportTile(ctx, st, t, res, warns)
		if err != nil {
			return nil, err
		}
		tiles.put(t.Slug, body)
	}
	if len(tiles) > 0 {
		env.put("tiles", tiles)
	}
	vols, err := d.Volumes.List(ctx, volume.Scope{Kind: "env", ID: e.ID})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(vols, func(a, b store.Volume) int { return cmp.Compare(a.Slug, b.Slug) })
	vm := omap{}
	for _, v := range vols {
		if v.InstanceID != nil || v.OrphanedAt != nil {
			continue
		}
		body := omap{}
		if v.MaxSizeMB > 0 {
			body.put("max_size_mb", v.MaxSizeMB)
		}
		vm.put(v.Slug, body)
	}
	if len(vm) > 0 {
		env.put("volumes", vm)
	}
	return env, nil
}

// defaultsMap is a settings blob as the defaults: keys that are set.
// A literal basic-auth pair is a secret and is left out (the plan reads an
// omitted pair as untouched).
func defaultsMap(blob string) map[string]any {
	s, err := settings.Parse(blob)
	if err != nil {
		return nil
	}
	b, _ := json.Marshal(Defaults(s))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if pw, ok := m["protect_password"].(string); ok && !strings.HasPrefix(strings.TrimSpace(pw), "${{") {
		delete(m, "protect_user")
		delete(m, "protect_password")
	}
	return m
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func jsonStrings(blob string) map[string]string {
	var m map[string]string
	if strings.TrimSpace(blob) != "" {
		_ = json.Unmarshal([]byte(blob), &m)
	}
	return m
}

// exportTile is toRow backwards: every key the row sets that the file would
// not default to.
func (f *Flow) exportTile(ctx context.Context, st store.Stack, t store.Tile, res []store.DomainResource, warns *[]string) (omap, error) {
	b := omap{}
	str := func(k, v string) {
		if v != "" {
			b.put(k, v)
		}
	}
	num := func(k string, v int) {
		if v != 0 {
			b.put(k, v)
		}
	}
	list := func(k, v string) {
		if l := splitLines(v); len(l) > 0 {
			b.put(k, l)
		}
	}
	if t.Kind != tile.Service && t.Kind != tile.Image && t.Kind != tile.Managed {
		b.put("type", t.Kind)
	}
	if t.Kind == tile.Managed {
		m, err := f.D.Managed.GetByTile(ctx, t.ID)
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return nil, err
		}
		str("engine", m.Engine)
		if len(m.Allow) > 0 {
			b.put("allow", []string(m.Allow))
		}
		if len(m.EnvPairs) > 0 {
			b.put("env_pairs", map[string]string(m.EnvPairs))
		}
	}
	str("image", t.ImageRef)
	if tile.Builds(t) {
		if t.GitURL != st.ConfigRepo {
			str("git_url", t.GitURL)
		}
		if t.GitBranch != cmp.Or(st.ConfigBranch, "main") {
			str("branch", t.GitBranch)
		}
		bd := omap{}
		if t.BuildContext != "" && t.BuildContext != "." {
			bd.put("context", t.BuildContext)
		}
		if t.DockerfilePath != "" && t.DockerfilePath != "Dockerfile" {
			bd.put("dockerfile", t.DockerfilePath)
		}
		if len(bd) > 0 {
			b.put("build", bd)
		}
		if m := jsonStrings(t.BuildArgs); len(m) > 0 {
			b.put("build_args", m)
		}
		list("watch_paths", t.WatchPaths)
	}
	if t.UpdatePolicy == "auto" {
		b.put("update_policy", "auto")
	}
	str("tag_policy", t.TagPolicy)
	str("command", t.Command)
	num("port", t.ContainerPort)
	list("published_ports", t.PublishedPorts)
	str("endpoint_protocol", t.EndpointProtocol)
	str("health_path", t.HealthPath)
	str("healthcheck", t.HealthcheckCmd)
	num("healthcheck_interval", t.HealthcheckIntervalS)
	num("healthcheck_timeout", t.HealthcheckTimeoutS)
	num("healthcheck_retries", t.HealthcheckRetries)
	num("healthcheck_start_period", t.HealthcheckStartPeriodS)
	if t.CPULimit != 0 || t.MemLimitMB != 0 {
		l := omap{}
		if t.CPULimit != 0 {
			l.put("cpu", t.CPULimit)
		}
		if t.MemLimitMB != 0 {
			l.put("memory_mb", t.MemLimitMB)
		}
		b.put("limits", l)
	}
	str("user", t.User)
	num("shm_size_mb", t.ShmSizeMB)
	if t.Privileged {
		b.put("privileged", true)
	}
	list("devices", t.Devices)
	list("lan", t.Lan)
	if t.HostNetwork {
		b.put("network", "host")
	}
	if t.RestartPolicy != "" && t.RestartPolicy != "always" && t.RestartPolicy != "unless-stopped" {
		b.put("restart", t.RestartPolicy)
	}
	list("depends_on", t.DependsOn)
	list("files", t.Files)
	list("volumes", t.Volumes)
	if t.Replicas > 1 {
		b.put("replicas", t.Replicas)
	}
	if m := jsonStrings(t.EnvJSON); len(m) > 0 {
		b.put("env", m)
	}
	str("schedule", t.Schedule)
	if t.Trigger != "" && t.Trigger != tile.Manual {
		b.put("trigger", t.Trigger)
	}
	if t.TimeoutMinutes != tile.DefaultTimeout {
		num("timeout_minutes", t.TimeoutMinutes)
	}
	if t.Kind == tile.Slice {
		str("provision_from", deref(t.ProvisionFrom))
		if a := deref(t.DefaultAccess); a != tile.Write {
			str("default_access", a)
		}
		if o := deref(t.OnRemove); o != tile.Keep {
			str("on_remove", o)
		}
	}
	if len(t.SliceAccess) > 0 {
		var sa []any
		for _, a := range t.SliceAccess {
			sa = append(sa, omap{{"from", a.From}, {"access", a.Access}})
		}
		b.put("slice_access", sa)
	}
	if t.Kind == tile.Managed || t.Kind == tile.Slice {
		return b, nil
	}
	have, err := f.D.Domains.ListByTile(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(have, func(a, c store.Domain) int { return cmp.Compare(a.Position, c.Position) })
	var ds []any
	for _, row := range have {
		if x, w, ok := exportDomain(row, t, res); ok {
			ds = append(ds, x)
			if w != "" {
				*warns = append(*warns, fmt.Sprintf("domain %s: %s", row.Host, w))
			}
		}
	}
	if len(ds) > 0 {
		b.put("domains", ds)
	}
	return b, nil
}

// exportDomain is one claim; false for a generated redirect (no file row)
// and a literal basic-auth password is left out with a warning (it would be a
// value in git); planning the file then shows a basic_auth change.
func exportDomain(row store.Domain, t store.Tile, res []store.DomainResource) (omap, string, bool) {
	var warn string
	if row.Auto && row.RedirectTo != "" {
		return nil, "", false
	}
	x := omap{}
	switch {
	case row.Auto:
		x.put("auto", true)
	case row.ResourceID != nil:
		i := slices.IndexFunc(res, func(r store.DomainResource) bool { return r.ID == *row.ResourceID })
		if i < 0 {
			x.put("host", row.Host)
			break
		}
		x.put("apex", res[i].Host)
	default:
		x.put("host", row.Host)
	}
	if row.Path != "" {
		x.put("path", row.Path)
	}
	if row.ContainerPort != 0 && row.ContainerPort != t.ContainerPort {
		x.put("port", row.ContainerPort)
	}
	if !row.HTTPS {
		x.put("https", false)
	}
	if !row.ForceHTTPS {
		x.put("force_https", false)
	}
	if row.RedirectTo != "" {
		x.put("redirect_to", row.RedirectTo)
	}
	var ex domain.Extras
	if strings.TrimSpace(row.ProxyJSON) != "" {
		_ = json.Unmarshal([]byte(row.ProxyJSON), &ex)
	}
	p := omap{}
	if a := ex.BasicAuth; a != nil {
		if strings.HasPrefix(strings.TrimSpace(a.Password), "${{") {
			p.put("basic_auth", omap{{"user", a.User}, {"password", a.Password}})
		} else {
			warn = "basic auth password is not a param ref; left out, planning this file removes the basic auth, add it by hand"
		}
	}
	if ex.Websockets {
		p.put("websockets", true)
	}
	if ex.MaxBodyMB != 0 {
		p.put("max_body_mb", ex.MaxBodyMB)
	}
	if ti := ex.Timeouts; ti != nil {
		to := omap{}
		for _, k := range []struct {
			n string
			v int
		}{{"dial", ti.Dial}, {"read", ti.Read}, {"write", ti.Write}} {
			if k.v != 0 {
				to.put(k.n, k.v)
			}
		}
		p.put("timeouts", to)
	}
	if len(ex.Headers) > 0 {
		p.put("headers", ex.Headers)
	}
	if len(ex.Methods) > 0 {
		p.put("methods", ex.Methods)
	}
	if ex.StripPrefix {
		p.put("strip_prefix", true)
	}
	if ex.SecHeaders {
		p.put("security_headers", true)
	}
	if fa := ex.ForwardAuth; fa != nil {
		w := omap{{"url", fa.URL}}
		if len(fa.CopyHeaders) > 0 {
			w.put("copy_headers", fa.CopyHeaders)
		}
		p.put("forward_auth", w)
	}
	if len(p) > 0 {
		x.put("proxy", p)
	}
	return x, warn, true
}
