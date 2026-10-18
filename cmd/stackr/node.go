package main

import (
	"encoding/json"
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

var nodeTags = []string{"ingress", "egress", "builder", "storage"}

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
		a.nodeAdd(), a.nodeConfirm(), a.nodeReject(), a.nodeLs(), a.nodeShow(), a.nodeMetrics(),
		a.nodeTag(true), a.nodeTag(false), a.nodeExit(),
		a.nodeRejoin(), a.nodeRotate(), a.nodeRemove(), a.nodeForget(), a.nodeBackup(), a.nodeBackups())
}

func (a *app) nodeAdd() *cobra.Command {
	var tags []string
	c := leaf("add <name>", "admin.join-token", "Make the join command for a new worker; it always gets egress (server admin)", exact(1),
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
			a.sayJoin(fmt.Sprintf("Join %s to this cluster. On the new machine, as root, run:", args[0]), asMap(v))
			return nil
		})
	c.Long = "Make the join command for a new worker (server admin). The node always gets egress; add more tags with --tag.\n" +
		"Orgs reach it through a placement: stackr admin placement add <name> --node <node>."
	c.Example = "  stackr node add vps-fsn1 --tag ingress\n  stackr node add vps-fsn1 --tag storage"
	c.Flags().StringArrayVar(&tags, "tag", nil, "another capability: ingress, builder or storage (repeatable; egress is always on)")
	return c
}

// sayJoin prints a minted join command: the one line, or the two-step
// version when the panel needs it, and when the token runs out.
func (a *app) sayJoin(head string, m map[string]any) {
	line, _ := m["one_line"].(string)
	if two, _ := m["two_step"].(string); two != "" {
		head += " (the panel has no trusted certificate, so it takes two steps)"
		line = strings.ReplaceAll(two, "\n", "\n  ")
	}
	a.say("%s\n\n  %s\n", head, line)
	if t, err := time.Parse(time.RFC3339, cell(m["expires_at"])); err == nil {
		a.say("The token works once and expires in %d minutes.", int(time.Until(t).Round(time.Minute).Minutes()))
	}
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
			who := cell(p["name"])
			if len(sys) > 0 {
				who += " (" + strings.Join(sys, ", ") + ")"
			}
			if err := a.confirm("Add " + who + " to the cluster? It will run what you pin to it."); err != nil {
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

// stateWord is a node state as people read it; --json keeps the raw word.
func stateWord(s string) string {
	switch s {
	case "tunnel-down":
		return "tunnel down, host alive"
	case "cert-expired":
		return "certificate expired: rejoin"
	case "not-rejoined":
		return "not rejoined: rejoin"
	}
	return s
}

// warnings is one field of each of a node's warnings.
func warnings(n map[string]any, field string) []string {
	var out []string
	for _, w := range asList(n["warnings"]) {
		out = append(out, cell(asMap(w)[field]))
	}
	return out
}

func (a *app) nodeLs() *cobra.Command {
	var state string
	var pend bool
	c := leaf("ls", "node.list,admin.join-requests", "List nodes (server admin)", exact(0), func(*cobra.Command, []string) error {
		if pend || state == "pending" {
			return a.pendingLs()
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
			m["state"] = stateWord(cell(m["state"]))
			m["tags"] = strings.Join(strs(m["tags"]), ",")
			m["mem"], m["disk_free"] = gb(m["mem_total"]), gb(m["disk_free"])
			if num(m["cpu"]) == 0 {
				m["cpu"] = "-"
			}
			if cell(m["docker"]) == "(none)" {
				m["docker"] = "-"
			}
			m["warn"] = "-"
			if ws := warnings(m, "label"); len(ws) > 0 {
				m["warn"] = strings.Join(ws, ", ")
			}
		}
		return a.show(rows, "name", "role", "state", "tags", "envs", "cpu", "mem", "disk_free", "docker", "warn")
	})
	c.Example = "  stackr node ls\n  stackr node ls --pending"
	c.Flags().StringVar(&state, "state", "", "only nodes in this state (online, pending, ...)")
	c.Flags().BoolVar(&pend, "pending", false, "only nodes waiting for stackr node confirm")
	return c
}

// pendingLs lists the workers waiting for confirm. They are join requests,
// not nodes yet: /nodes, like the panel's list, leaves them out.
func (a *app) pendingLs() error {
	v, err := a.call(GET, "/admin/join-requests", nil)
	if err != nil {
		return err
	}
	rows := asList(v)
	if a.json {
		return a.show(rows)
	}
	for _, r := range rows {
		m := asMap(r)
		facts := asMap(m["facts"])
		m["tags"] = strings.Join(strs(m["tags"]), ",")
		m["cpu"], m["mem"] = facts["cpus"], gb(facts["mem_bytes"])
		if num(m["cpu"]) == 0 {
			m["cpu"] = "-"
		}
	}
	return a.show(rows, "name", "tags", "cpu", "mem", "requested_at")
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
				{"state", stateWord(cell(n["state"]))},
				{"tags", strings.Join(strs(n["tags"]), ", ")},
			}
			if ar := cell(n["arch"]); ar != "(none)" {
				rows = append(rows, []string{"arch", ar})
			}
			if v := cell(n["version"]); v != "(none)" {
				if t, _ := n["too_old"].(bool); t {
					v += " (too old)"
				} else if b, _ := n["behind"].(bool); b {
					v += " (behind)"
				}
				rows = append(rows, []string{"stackr", v})
			}
			if f := asMap(n["facts"]); f != nil {
				rows = append(rows,
					[]string{"version", fmt.Sprintf("Docker %s, kernel %s", cell(f["docker"]), cell(f["kernel"]))},
					[]string{"system", fmt.Sprintf("%.0f CPU, %s memory, %s of %s disk free, up %.0f days",
						num(f["cpus"]), gb(f["mem_bytes"]), gb(f["disk_free"]), gb(f["disk_total"]), num(f["uptime_s"])/86400)})
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
			if ps := strs(n["placements"]); len(ps) > 0 {
				rows = append(rows, []string{"placements", strings.Join(ps, ", ")})
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
			if de := strs(n["default_exit"]); len(de) > 0 {
				rows = append(rows, []string{"default exit", strings.Join(de, ", ")})
			}
			if ig := strs(n["ingress_for"]); len(ig) > 0 {
				rows = append(rows, []string{"ingress for", strings.Join(ig, ", ")})
			}
			if cert := asMap(n["certificate"]); cert != nil {
				rows = append(rows, []string{"certificate", fmt.Sprintf("valid until %s, renews on %s",
					cell(cert["not_after"])[:10], cell(cert["renews_on"])[:10])})
			}
			for _, w := range warnings(n, "detail") {
				rows = append(rows, []string{"warning", w})
			}
			a.table(nil, rows)
			return nil
		})
}

func (a *app) nodeTag(add bool) *cobra.Command {
	use, op, short := "tag <name> <tag>...", "admin.node-tags", "Add capability tags (ingress, egress, builder, storage) (server admin)"
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

// nodeExit sets where a node without egress sends its internet traffic. The
// service refuses a clear on such a node and exits that lack egress.
func (a *app) nodeExit() *cobra.Command {
	c := leaf("exit <name> [<exit>...]", "admin.node-exit",
		"Set the exits a node without egress sends its traffic out through, in order, up to three (server admin)", atLeast(1),
		func(_ *cobra.Command, args []string) error {
			name, exits := args[0], args[1:]
			v, err := a.call(PUT, "/admin/nodes/"+url.PathEscape(name)+"/exit", map[string][]string{"exits": exits})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			if len(exits) == 0 {
				a.say("%s: default exit cleared", name)
				return nil
			}
			a.say("%s: default exit %s", name, strings.Join(exits, ", "))
			return nil
		})
	c.Example = "  stackr node exit vps-home vps-fsn1 vps-hel1"
	return c
}

// orList is "acme, shop", or "no org (admins only)" for none.
func orList(orgs []string) string {
	if len(orgs) == 0 {
		return "no org (admins only)"
	}
	return strings.Join(orgs, ", ")
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
			a.sayJoin(fmt.Sprintf("%s will need its install command run again. It keeps its envs and data. On %s, as root:", args[0], args[0]), asMap(v))
			return nil
		})
}

func (a *app) nodeRotate() *cobra.Command {
	var all bool
	every := -1
	c := leaf("rotate [<name>]", "admin.node-rotate,admin.nodes-rotate,admin.key-rotation,admin.key-rotation-set",
		"Replace a node's mesh keys without a rejoin; --all for every node, --every to schedule (server admin)",
		cobra.MaximumNArgs(1),
		func(_ *cobra.Command, args []string) error {
			if every >= 0 {
				v, err := a.call(PUT, "/admin/key-rotation", map[string]any{"days": every})
				if err != nil {
					return err
				}
				if a.json {
					return a.show(v)
				}
				if every == 0 {
					a.say("Scheduled key rotation is off.")
				} else {
					a.say("Every node's keys rotate every %d days, one node at a time.", every)
				}
				if len(args) == 0 && !all {
					return nil
				}
			}
			switch {
			case all && len(args) == 0:
				if _, err := a.call(POST, "/admin/nodes/rotate", nil); err != nil {
					return err
				}
				a.say("Rotating every node's keys, one at a time. Each shows in the security log (stackr admin audit).")
			case len(args) == 1 && !all:
				if _, err := a.call(POST, "/admin/nodes/"+url.PathEscape(args[0])+"/rotate", nil); err != nil {
					return err
				}
				a.say("Rotating %s's keys. Its tunnels blip for a moment; the security log shows when it is done.", args[0])
			case every < 0:
				v, err := a.call(GET, "/admin/key-rotation", nil)
				if err != nil {
					return err
				}
				if a.json {
					return a.show(v)
				}
				if d := cell(asMap(v)["days"]); d != "0" {
					a.say("Keys rotate every %s days. Give a node name or --all to rotate now.", d)
				} else {
					a.say("Scheduled key rotation is off. Give a node name or --all to rotate now.")
				}
			default:
				return fmt.Errorf("give a node name or --all, not both")
			}
			return nil
		})
	c.Example = "  stackr node rotate h3\n  stackr node rotate --all\n  stackr node rotate --every 7"
	c.Flags().BoolVar(&all, "all", false, "every node, one at a time")
	c.Flags().IntVar(&every, "every", -1, "rotate every N days (0 = off)")
	return c
}

func (a *app) nodeRemove() *cobra.Command {
	c := leaf("remove <name>", "admin.node-remove", "Take a healthy worker out, once no placement points at it (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			name := args[0]
			n, err := a.node(name)
			if err != nil {
				return err
			}
			if cell(n["role"]) == "controller" {
				return fmt.Errorf("%s is the controller; it cannot be removed", name)
			}
			if ps := strs(n["placements"]); len(ps) > 0 {
				return fmt.Errorf("%s hosts placement %s. Repoint it first: stackr admin placement move <placement> <node>", name, strings.Join(ps, ", "))
			}
			if envs := asList(n["env_list"]); len(envs) > 0 {
				return fmt.Errorf("%s runs %s. Repoint its placements first: stackr admin placement move <placement> <node>", name, plural(len(envs), "env", "envs"))
			}
			if err := a.confirm("Remove " + name + " from the cluster? Stops everything on it, backs it up, checks the backups, then cleans the box and removes it. A failed backup stops there and starts the apps again."); err != nil {
				return err
			}
			j, err := a.call(DELETE, "/admin/nodes/"+url.PathEscape(name), nil)
			if err != nil {
				return err
			}
			return a.follow("/admin", j, "removal of "+name)
		})
	c.Example = "  stackr node remove vps-fsn1"
	return c
}

// nodeBackup backs up every volume on a node as one group (server admin).
func (a *app) nodeBackup() *cobra.Command {
	return leaf("backup <name>", "admin.node-backup", "Back up every volume on a node, one env at a time (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := a.confirm("Back up " + name + "? Each env's apps stop while its volumes are copied, then start again."); err != nil {
				return err
			}
			j, err := a.call(POST, "/admin/nodes/"+url.PathEscape(name)+"/backup", nil)
			if err != nil {
				return err
			}
			return a.follow("/admin", j, "backup of "+name)
		})
}

// nodeBackups lists a node's backup groups with their volumes (server admin).
func (a *app) nodeBackups() *cobra.Command {
	return leaf("backups <name>", "admin.node-backups", "List a node's backups, newest first (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			v, err := a.call(GET, "/admin/nodes/"+url.PathEscape(args[0])+"/backups", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			for _, g := range asList(v) {
				m := asMap(g)
				a.say("%s\t%s\t%s", cell(m["label"]), cell(m["status"]), cell(m["dest"]))
				if w := cell(m["warning"]); w != "" && w != "(none)" {
					a.say("  %s", w)
				}
				if e := cell(m["error"]); e != "" && e != "(none)" {
					a.say("  %s", e)
				}
				for _, r := range asList(m["volumes"]) {
					rm := asMap(r)
					a.say("  %s\t%s\t%s\t%s", cell(rm["volume"]), cell(rm["method"]), cell(rm["status"]), cell(rm["error"]))
				}
			}
			return nil
		})
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
			b.WriteString("\nForgetting removes " + name + " from the cluster for good: its certificate is revoked, its tunnel removed, its address range retired. If it still answers, its containers, networks and tunnel come off the box first; volumes stay.")
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
			// a node still online is cleaned by a job first
			if j := asMap(v)["job"]; j != nil {
				if err := a.follow("/admin", j, "forget of "+name); err != nil {
					return err
				}
			}
			lost := asList(asMap(v)["lost"])
			a.say("%s: forgotten; %s marked lost", name, plural(len(lost), "env", "envs"))
			if len(lost) > 0 {
				a.say("Bring them back: stackr env restore --placement <placement> --to <node>   (stackr env lost lists them)")
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
						rtt := ""
						if n, _ := lm["ms"].(json.Number); n != "" {
							ms, _ := n.Float64()
							rtt = fmt.Sprintf("  %.1f ms", ms)
						}
						a.say("  %-9s <-> %-9s %-8s %s%s", cell(lm["a"]), cell(lm["b"]), cell(lm["path"]), cell(lm["state"]), rtt)
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
	return noun("cluster", "The cluster at a glance, and its root of trust", st,
		a.clusterTrust(), a.clusterRotateRoot(), a.clusterBreakGlass(), a.clusterRevokeControllerCert())
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
