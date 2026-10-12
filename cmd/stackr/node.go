package main

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The multi-node commands: nodes, the cluster view and the registry login.
// Nodes are addressed by name. Every write prints one line saying what
// changed; --json prints the API's object.

var nodeTags = []string{"ingress", "egress", "builder"}

// ---- small formatters ----

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func asList(v any) []any { l, _ := v.([]any); return l }

func strs(v any) []string {
	var out []string
	for _, x := range asList(v) {
		out = append(out, cell(x))
	}
	return out
}

func num(v any) float64 {
	switch n := v.(type) {
	case interface{ Float64() (float64, error) }:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return 0
}

func gb(v any) string {
	b := num(v)
	if b == 0 {
		return "-"
	}
	g := b / (1 << 30)
	if g < 10 {
		return fmt.Sprintf("%.1f GB", g)
	}
	return fmt.Sprintf("%.0f GB", g)
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (a *app) node(name string) (map[string]any, error) {
	v, err := a.call(GET, "/nodes/"+url.PathEscape(name), nil)
	return asMap(v), err
}

func (a *app) nodeNames() (map[string]string, error) {
	v, err := a.call(GET, "/nodes", nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, n := range asList(v) {
		m := asMap(n)
		out[cell(m["id"])] = cell(m["name"])
	}
	return out, nil
}

// ---- node ----

func (a *app) nodes() *cobra.Command {
	return noun("node", "The cluster's machines: the controller and its workers",
		a.nodeAdd(), a.nodeConfirm(), a.nodeReject(), a.nodeLs(), a.nodeShow(),
		a.nodeTag(true), a.nodeTag(false), a.nodeOpen(true), a.nodeOpen(false),
		a.nodeRejoin(), a.nodeRemove(), a.nodeForget())
}

func (a *app) nodeAdd() *cobra.Command {
	var tags []string
	c := leaf("add <name>", "admin.join-token", "Make the join command for a new worker (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			for _, t := range tags {
				if !slices.Contains(nodeTags, t) {
					return usage("unknown tag %q; tags are %s", t, strings.Join(nodeTags, ", "))
				}
			}
			v, err := a.call(POST, "/admin/join-tokens", map[string]any{"node_name": args[0], "tags": tags})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			m := asMap(v)
			a.say("Join %s to this cluster. On the new machine, as root, run:\n\n  %s\n", args[0], cell(m["command"]))
			if t, err := time.Parse(time.RFC3339, cell(m["expires_at"])); err == nil {
				a.say("The token works once and expires in %d minutes.", int(time.Until(t).Round(time.Minute).Minutes()))
			}
			return nil
		})
	c.Example = "  stackr node add vps-fsn1 --tag ingress --tag egress"
	c.Flags().StringArrayVar(&tags, "tag", nil, "a capability: ingress, egress or builder (repeatable)")
	return c
}

// pending finds the join request of a node by name.
func (a *app) pending(name string) (map[string]any, error) {
	v, err := a.call(GET, "/admin/join-requests", nil)
	if err != nil {
		return nil, err
	}
	for _, p := range asList(v) {
		if m := asMap(p); cell(m["name"]) == name || cell(m["node_id"]) == name {
			return m, nil
		}
	}
	if n, err := a.node(name); err == nil {
		if st, _ := n["state"].(string); st != "" && st != "pending" {
			return nil, fmt.Errorf("%s is already %s", name, st)
		}
	}
	return nil, fmt.Errorf("no pending node %q; stackr node ls --pending lists them", name)
}

func (a *app) nodeConfirm() *cobra.Command {
	return leaf("confirm <name>", "admin.join-requests,admin.join-confirm", "Accept a worker that called back (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			p, err := a.pending(args[0])
			if err != nil {
				return err
			}
			facts := asMap(p["facts"])
			sys := []string{}
			if n := num(facts["cpus"]); n > 0 {
				sys = append(sys, fmt.Sprintf("%.0f CPU", n))
			}
			if b := num(facts["mem_bytes"]); b > 0 {
				sys = append(sys, gb(b))
			}
			if err := a.confirm(fmt.Sprintf("Add %s (%s) to the cluster? It will run what you pin to it.", cell(p["name"]), strings.Join(sys, ", "))); err != nil {
				return err
			}
			v, err := a.call(POST, "/admin/join-requests/"+cell(p["node_id"])+"/confirm", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			a.say("%s: confirmed; it comes online when its worker connects", args[0])
			return nil
		})
}

func (a *app) nodeReject() *cobra.Command {
	return leaf("reject <name>", "admin.join-requests,admin.join-reject", "Turn a worker that called back away (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			p, err := a.pending(args[0])
			if err != nil {
				return err
			}
			if err := a.confirm("Turn " + cell(p["name"]) + " away? Its join token is spent."); err != nil {
				return err
			}
			if _, err := a.call(POST, "/admin/join-requests/"+cell(p["node_id"])+"/reject", nil); err != nil {
				return err
			}
			a.say("%s: rejected", args[0])
			return nil
		})
}

func (a *app) nodeLs() *cobra.Command {
	var state string
	var pend bool
	c := leaf("ls", "node.list", "List nodes", exact(0), func(*cobra.Command, []string) error {
		if pend {
			state = "pending"
		}
		v, err := a.call(GET, "/nodes", nil)
		if err != nil {
			return err
		}
		rows := []any{}
		for _, n := range asList(v) {
			if m := asMap(n); state == "" || cell(m["state"]) == state {
				rows = append(rows, m)
			}
		}
		if a.json {
			return a.show(rows)
		}
		for _, r := range rows {
			m := asMap(r)
			m["tags"] = strings.Join(strs(m["tags"]), ",")
			m["mem"], m["disk_free"] = gb(m["mem_total"]), gb(m["disk_free"])
			if num(m["cpu"]) == 0 {
				m["cpu"] = "-"
			}
			if cell(m["docker"]) == "(none)" {
				m["docker"] = "-"
			}
		}
		return a.show(rows, "name", "role", "state", "tags", "envs", "cpu", "mem", "disk_free", "docker")
	})
	c.Example = "  stackr node ls\n  stackr node ls --pending"
	c.Flags().StringVar(&state, "state", "", "only nodes in this state (online, pending, ...)")
	c.Flags().BoolVar(&pend, "pending", false, "only nodes waiting for stackr node confirm")
	return c
}

func (a *app) nodeShow() *cobra.Command {
	return leaf("show <name>", "node.get", "Show one node: system, tags, certificate, envs", exact(1),
		func(_ *cobra.Command, args []string) error {
			n, err := a.node(args[0])
			if err != nil {
				return err
			}
			if a.json {
				return a.show(n)
			}
			rows := [][]string{
				{"name", cell(n["name"])},
				{"role", cell(n["role"])},
				{"state", cell(n["state"])},
				{"tags", strings.Join(strs(n["tags"]), ", ")},
			}
			if f := asMap(n["facts"]); f != nil {
				rows = append(rows,
					[]string{"version", fmt.Sprintf("Docker %s, kernel %s", cell(f["docker"]), cell(f["kernel"]))},
					[]string{"system", fmt.Sprintf("%.0f CPU, %s memory, %s disk free, up %.0f days",
						num(f["cpus"]), gb(f["mem_bytes"]), gb(f["disk_free"]), num(f["uptime_s"])/86400)})
				var addrs []string
				for _, i := range asList(f["interfaces"]) {
					im := asMap(i)
					kind := "private"
					if b, _ := im["public"].(bool); b {
						kind = "public"
					}
					addrs = append(addrs, fmt.Sprintf("%s (%s, %s)", strings.Join(strs(im["prefixes"]), " "), kind, cell(im["name"])))
				}
				rows = append(rows, []string{"addresses", strings.Join(addrs, "   ")})
			}
			if r := cell(n["range"]); r != "(none)" {
				rows = append(rows, []string{"range", r})
			}
			if o := strs(n["open_to"]); len(o) > 0 {
				rows = append(rows, []string{"open to", strings.Join(o, ", ")})
			}
			envs := []string{}
			for _, e := range asList(n["env_list"]) {
				envs = append(envs, cell(asMap(e)["path"]))
			}
			if len(envs) == 0 {
				envs = append(envs, "(none)")
			}
			rows = append(rows, []string{"envs", strings.Join(envs, ", ")})
			if ex := strs(n["exit_for"]); len(ex) > 0 {
				rows = append(rows, []string{"exit for", strings.Join(ex, ", ")})
			}
			if ig := strs(n["ingress_for"]); len(ig) > 0 {
				rows = append(rows, []string{"ingress for", strings.Join(ig, ", ")})
			}
			if cert := asMap(n["certificate"]); cert != nil {
				rows = append(rows, []string{"certificate", fmt.Sprintf("valid until %s, renews on %s",
					cell(cert["not_after"])[:10], cell(cert["renews_on"])[:10])})
			}
			a.table(nil, rows)
			return nil
		})
}

func (a *app) nodeTag(add bool) *cobra.Command {
	use, op, short := "tag <name> <tag>...", "admin.node-tags", "Add capability tags (ingress, egress, builder) (server admin)"
	if !add {
		use, short = "untag <name> <tag>...", "Remove capability tags (server admin)"
	}
	c := leaf(use, op, short, atLeast(2), func(_ *cobra.Command, args []string) error {
		name, tags := args[0], args[1:]
		for _, t := range tags {
			if !slices.Contains(nodeTags, t) {
				return usage("unknown tag %q; tags are %s", t, strings.Join(nodeTags, ", "))
			}
		}
		body := map[string][]string{"add": tags, "remove": {}}
		if !add {
			body = map[string][]string{"add": {}, "remove": tags}
			if slices.Contains(tags, "egress") {
				n, err := a.node(name)
				if err != nil {
					return err
				}
				if ex := strs(n["exit_for"]); len(ex) > 0 {
					if err := a.confirm(fmt.Sprintf("%s is the exit for %s: %s. Remove egress? They move to the next exit in their list, or lose internet if there is none.",
						name, plural(len(ex), "scope", "scopes"), strings.Join(ex, ", "))); err != nil {
						return err
					}
				}
			}
		}
		v, err := a.call(PUT, "/admin/nodes/"+url.PathEscape(name)+"/tags", body)
		if err != nil {
			return err
		}
		if a.json {
			return a.show(v)
		}
		a.say("%s: tags %s", name, strings.Join(strs(asMap(v)["tags"]), ", "))
		return nil
	})
	c.Example = "  stackr node " + strings.Fields(use)[0] + " vps-fsn1 egress"
	return c
}

func (a *app) nodeOpen(open bool) *cobra.Command {
	use, short := "open <name>", "Let an org pin to a node (server admin)"
	if !open {
		use, short = "close <name>", "Stop an org from pinning to a node; its envs there stay (server admin)"
	}
	var orgSlug string
	c := leaf(use, "admin.node-open,admin.node-close", short, exact(1), func(*cobra.Command, []string) error { return nil })
	c.RunE = func(_ *cobra.Command, args []string) error {
		if orgSlug == "" {
			p, err := a.orgPath()
			if err != nil {
				return err
			}
			orgSlug = last(p)
		}
		name := args[0]
		if open {
			v, err := a.call(POST, "/admin/nodes/"+url.PathEscape(name)+"/orgs", map[string]string{"org": orgSlug})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			a.say("%s: open to %s", name, orgSlug)
			return nil
		}
		n, err := a.node(name)
		if err != nil {
			return err
		}
		cnt := 0
		for _, e := range asList(n["env_list"]) {
			if cell(asMap(e)["org"]) == orgSlug {
				cnt++
			}
		}
		if cnt > 0 {
			if err := a.confirm(fmt.Sprintf("%s runs %d %s envs; they stay, new ones cannot land there. Close it to %s?", name, cnt, orgSlug, orgSlug)); err != nil {
				return err
			}
		}
		v, err := a.call(DELETE, "/admin/nodes/"+url.PathEscape(name)+"/orgs/"+url.PathEscape(orgSlug), nil)
		if err != nil {
			return err
		}
		if a.json {
			return a.show(v)
		}
		a.say("%s: closed to %s", name, orgSlug)
		return nil
	}
	c.Example = "  stackr node " + strings.Fields(use)[0] + " vps-fsn1 --org acme"
	c.Flags().StringVar(&orgSlug, "org", "", "the org (default: the current one)")
	return c
}

func (a *app) nodeRejoin() *cobra.Command {
	return leaf("rejoin <name>", "admin.node-rejoin", "New join command for a broken worker; it keeps its envs (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			v, err := a.call(POST, "/admin/nodes/"+url.PathEscape(args[0])+"/rejoin", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			a.say("%s will need its install command run again. It keeps its envs and data. On %s, as root:\n\n  %s\n", args[0], args[0], cell(asMap(v)["command"]))
			if t, err := time.Parse(time.RFC3339, cell(asMap(v)["expires_at"])); err == nil {
				a.say("The token works once and expires in %d minutes.", int(time.Until(t).Round(time.Minute).Minutes()))
			}
			return nil
		})
}

func (a *app) nodeRemove() *cobra.Command {
	var moveTo string
	c := leaf("remove <name>", "admin.node-remove,env.move", "Take a healthy worker out, after moving its envs (server admin)", exact(1),
		func(c *cobra.Command, args []string) error {
			name := args[0]
			n, err := a.node(name)
			if err != nil {
				return err
			}
			envs := asList(n["env_list"])
			if len(envs) > 0 && moveTo == "" {
				return fmt.Errorf("%s runs %s. Move them first: stackr env move <env> --to <node>, or pass --move-to <node>", name, plural(len(envs), "env", "envs"))
			}
			q := "Remove " + name + " from the cluster? Its certificate is revoked and its tunnel removed."
			if len(envs) > 0 {
				q = fmt.Sprintf("Move %s to %s, then remove %s? Its certificate is revoked and its tunnel removed.", plural(len(envs), "env", "envs"), moveTo, name)
			}
			if err := a.confirm(q); err != nil {
				return err
			}
			for _, e := range envs {
				parts := strings.Split(cell(asMap(e)["path"]), "/")
				if len(parts) != 3 {
					return fmt.Errorf("cannot place env %q", cell(asMap(e)["path"]))
				}
				base := "/orgs/" + url.PathEscape(parts[0])
				j, err := a.call(POST, base+"/stacks/"+url.PathEscape(parts[1])+"/envs/"+url.PathEscape(parts[2])+"/move", map[string]any{"to": moveTo})
				if err != nil {
					return err
				}
				if err := a.follow(base, j, "move"); err != nil {
					return err
				}
			}
			if _, err := a.call(DELETE, "/admin/nodes/"+url.PathEscape(name), nil); err != nil {
				return err
			}
			a.say("%s: removed", name)
			return nil
		})
	c.Example = "  stackr node remove vps-fsn1 --move-to home"
	c.Flags().StringVar(&moveTo, "move-to", "", "move its envs to this node first")
	return c
}

func (a *app) nodeForget() *cobra.Command {
	return leaf("forget <name>", "admin.node-forget", "Write off a dead worker for good (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			name := args[0]
			n, err := a.node(name)
			if err != nil {
				return err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s is not coming back?", name)
			envs := asList(n["env_list"])
			if len(envs) > 0 {
				b.WriteString(" These envs ran on it and their data is there:")
			}
			for _, e := range envs {
				m := asMap(e)
				var vols []string
				for _, v := range asList(m["volumes"]) {
					vm := asMap(v)
					last := "no backup"
					if t, err := time.Parse(time.RFC3339, cell(vm["last_backup"])); err == nil {
						last = "last backup " + ago(t)
					}
					vols = append(vols, fmt.Sprintf("%s (%s)", cell(vm["name"]), last))
				}
				if len(vols) == 0 {
					vols = []string{"no volumes"}
				} else {
					vols[0] = "volumes " + vols[0]
				}
				fmt.Fprintf(&b, "\n  %s  %s", cell(m["path"]), strings.Join(vols, ", "))
			}
			b.WriteString("\nForgetting removes " + name + " from the cluster for good: its certificate is revoked, its tunnel removed, its address range retired.")
			if err := a.confirm(b.String()); err != nil {
				return err
			}
			v, err := a.call(POST, "/admin/nodes/"+url.PathEscape(name)+"/forget", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			lost := asList(asMap(v)["lost"])
			a.say("%s: forgotten; %s marked lost", name, plural(len(lost), "env", "envs"))
			if len(lost) > 0 {
				a.say("Bring them back: stackr env restore <env> --stack <stack> --to <node>   (stackr env lost lists them)")
			}
			return nil
		})
}

// ---- cluster ----

func (a *app) cluster() *cobra.Command {
	st := leaf("status", "cluster.status", "The whole cluster, grouped by network; exits 1 when anything is down", exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/cluster", nil)
			if err != nil {
				return err
			}
			m := asMap(v)
			problems := strs(m["problems"])
			if a.json {
				if err := a.show(v); err != nil {
					return err
				}
			} else {
				byName := map[string]map[string]any{}
				for _, n := range asList(m["nodes"]) {
					byName[cell(asMap(n)["name"])] = asMap(n)
				}
				line := func(name string) string {
					n := byName[name]
					return fmt.Sprintf("  %-10s %-10s %-8s %-24s %s", name, cell(n["role"]), cell(n["state"]),
						strings.Join(strs(n["tags"]), " "), plural(int(num(n["envs"])), "env", "envs"))
				}
				for _, g := range asList(m["groups"]) {
					gm := asMap(g)
					a.say("%s", strings.ToUpper(cell(gm["name"])))
					for _, n := range strs(gm["nodes"]) {
						a.say("%s", line(n))
					}
					a.say("")
				}
				if links := asList(m["links"]); len(links) > 0 {
					a.say("LINKS")
					for _, l := range links {
						lm := asMap(l)
						a.say("  %-9s <-> %-9s %-8s %s", cell(lm["a"]), cell(lm["b"]), cell(lm["path"]), cell(lm["state"]))
					}
					a.say("")
				}
				if len(problems) == 0 {
					a.say("all %s online", plural(len(byName), "node", "nodes"))
				} else {
					a.say("%s: %s", plural(len(problems), "problem", "problems"), strings.Join(problems, "; "))
				}
			}
			if len(problems) > 0 {
				return exitErr(1)
			}
			return nil
		})
	st.Example = "  stackr cluster status\n  stackr cluster status --json"
	return noun("cluster", "The cluster at a glance", st)
}

// ---- admin audit ----

func (a *app) adminAudit() *cobra.Command {
	var since, until, action, actor, target, outcome string
	var limit int
	c := leaf("audit", "admin.security-log,admin.users,node.list", "The security audit log (server admin)", exact(0),
		func(*cobra.Command, []string) error {
			q := url.Values{}
			for k, d := range map[string]string{"since": since, "until": until} {
				if d == "" {
					continue
				}
				t, err := timeBound(d)
				if err != nil {
					return usage("--%s: %v", k, err)
				}
				q.Set(k, t.Format(time.RFC3339))
			}
			if limit > 0 {
				q.Set("limit", fmt.Sprint(limit))
			}
			if outcome != "" && !slices.Contains([]string{"ok", "failed", "refused"}, outcome) {
				return usage("--outcome is ok, failed or refused")
			}
			path := "/admin/security-log"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			v, err := a.call(GET, path, nil)
			if err != nil {
				return err
			}
			users := map[string]string{}
			if us, err := a.call(GET, "/admin/users", nil); err == nil {
				for _, u := range asList(us) {
					um := asMap(u)
					users[cell(um["id"])] = cell(um["email"])
				}
			}
			nodes, _ := a.nodeNames()
			rows := []any{}
			for _, e := range asList(v) {
				m := asMap(e)
				who := "system"
				if id := cell(m["actor_id"]); id != "(none)" {
					who = id
					if em := users[id]; em != "" {
						who = em
					}
				} else if ip := cell(m["ip"]); ip != "(none)" {
					who = ip
				}
				if nm := nodes[cell(m["target_id"])]; nm != "" {
					m["target"] = nm
				} else {
					m["target"] = cell(m["target_id"])
				}
				m["actor"], m["outcome"] = who, auditOutcome(cell(m["action"]))
				if action != "" && !globMatch(action, cell(m["action"])) ||
					actor != "" && actor != who ||
					target != "" && target != cell(m["target"]) && target != cell(m["target_id"]) ||
					outcome != "" && outcome != m["outcome"] {
					continue
				}
				rows = append(rows, m)
			}
			if a.json {
				return a.show(rows)
			}
			for _, r := range rows {
				m := asMap(r)
				if t, err := time.Parse(time.RFC3339, cell(m["at"])); err == nil {
					m["time"] = t.Local().Format("Jan 02 15:04:05")
				}
			}
			return a.show(rows, "time", "actor", "action", "target", "outcome")
		})
	c.Example = "  stackr admin audit --since 24h\n  stackr admin audit --action 'cert.*' --outcome ok"
	c.Flags().StringVar(&since, "since", "", "from here on: a duration (24h) or an RFC 3339 time")
	c.Flags().StringVar(&until, "until", "", "up to here: a duration ago or an RFC 3339 time")
	c.Flags().StringVar(&action, "action", "", "an action, or a pattern like cert.*")
	c.Flags().StringVar(&actor, "actor", "", "an email, an address, or system")
	c.Flags().StringVar(&target, "target", "", "a node name or target id")
	c.Flags().StringVar(&outcome, "outcome", "", "ok, failed or refused")
	c.Flags().IntVar(&limit, "limit", 0, "at most this many events (newest first)")
	return c
}

// timeBound reads "24h" as that long ago, else an RFC 3339 time.
func timeBound(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, fmt.Errorf("%q is neither a duration like 24h nor an RFC 3339 time", s)
	}
	return t.UTC(), nil
}

func auditOutcome(action string) string {
	switch action {
	case "login.bad":
		return "failed"
	case "login.limited":
		return "refused"
	}
	return "ok"
}

// globMatch is a * pattern against s.
func globMatch(pat, s string) bool {
	if !strings.Contains(pat, "*") {
		return pat == s
	}
	parts := strings.Split(pat, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
