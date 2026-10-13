package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Placement: pin, ingress and exit work the same at every scope, so they are
// top-level nouns with --org, --stack, --env and --tile; env move and the
// lost-env verbs are under env (stack.go adds them).

// scopeFlags gives c the flags that name a scope. The deepest one given wins;
// none given means the linked directory, else the org.
func scopeFlags(c *cobra.Command, tile bool) *cobra.Command {
	c.Flags().String("org", "", "the org (default: the logged-in one)")
	c.Flags().String("stack", "", "the stack")
	c.Flags().String("env", "", "the environment")
	if tile {
		c.Flags().String("tile", "", "the tile (needs --stack and --env, or a linked directory)")
	}
	return c
}

// place is the API path of the scope the flags name.
func (a *app) place(c *cobra.Command, maxLevel level) (string, error) {
	if o := flag(c, "org"); o != "" {
		a.orgSlug = o
	}
	lv := atOrg
	switch {
	case flag(c, "tile") != "":
		lv = atTile
	case flag(c, "env") != "":
		lv = atEnv
	case flag(c, "stack") != "":
		lv = atStack
	case flag(c, "org") == "":
		_, l := a.here()
		switch {
		case l.Stack != "" && l.Env != "":
			lv = atEnv
		case l.Stack != "":
			lv = atStack
		}
	}
	if lv > maxLevel {
		return "", usage("--%s does not apply here", map[level]string{atEnv: "env", atTile: "tile"}[lv])
	}
	return a.path(c, lv, "")
}

// scopeName is the path as a person says it: acme/shop/prod.
func scopeName(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	var out []string
	for i := 1; i < len(segs); i += 2 {
		v, _ := url.PathUnescape(segs[i])
		out = append(out, v)
	}
	return strings.Join(out, "/")
}

func (a *app) pin() *cobra.Command {
	var clear bool
	c := leaf("pin [node]", "org.pin,org.pin-set,stack.pin,stack.pin-set", "Pin an org or stack to a node: where its new envs go", upTo(1),
		func(c *cobra.Command, args []string) error {
			if flag(c, "env") != "" {
				return usage("an env runs where it is; stackr env move moves it. Pin an org or a stack")
			}
			p, err := a.place(c, atStack)
			if err != nil {
				return err
			}
			if len(args) == 0 && !clear {
				v, err := a.call(GET, p+"/pin", nil)
				if err != nil {
					return err
				}
				if a.json {
					return a.show(v)
				}
				m := asMap(v)
				if cell(m["node"]) == "(none)" {
					a.say("%s: no pin; new envs go to the controller", cell(m["scope"]))
				} else {
					a.say("%s: new envs go to %s", cell(m["scope"]), cell(m["node"]))
				}
				return nil
			}
			if len(args) == 1 && clear {
				return usage("pass a node or --clear, not both")
			}
			node := ""
			if len(args) == 1 {
				node = args[0]
			}
			v, err := a.call(PUT, p+"/pin", map[string]string{"node": node})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			if node == "" {
				a.say("%s: pin removed; new envs follow the pin above, else the controller", scopeName(p))
			} else {
				a.say("%s: new envs go to %s. Existing envs stay where they are (stackr env move moves one).", scopeName(p), node)
			}
			return nil
		})
	c.Example = "  stackr pin vps-fsn1 --org acme\n  stackr pin vps-fsn1 --stack shop\n  stackr pin --clear --stack shop"
	c.Flags().BoolVar(&clear, "clear", false, "remove the pin")
	scopeFlags(c, false)
	return c
}

func (a *app) ingress() *cobra.Command {
	var clear, def bool
	var getDef bool
	get := leaf("get", "org.ingress,stack.ingress,env.ingress,tile.ingress,admin.node-defaults", "Which node public traffic enters through", exact(0),
		func(c *cobra.Command, _ []string) error {
			if getDef {
				return a.showDefaults()
			}
			p, err := a.place(c, atTile)
			if err != nil {
				return err
			}
			v, err := a.call(GET, p+"/ingress", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			m := asMap(v)
			a.say("%s\t%s\t(from %s)", cell(m["scope"]), cell(m["node"]), cell(m["from"]))
			return nil
		})
	get.Example = "  stackr ingress get --stack shop --env prod\n  stackr ingress get --default"
	get.Flags().BoolVar(&getDef, "default", false, "show the cluster-wide default ingress and exit")
	set := leaf("set [node]", "org.ingress-set,stack.ingress-set,env.ingress-set,tile.ingress-set,admin.default-ingress-set", "Pick the ingress node of a scope", upTo(1),
		func(c *cobra.Command, args []string) error {
			if def {
				return a.setDefault(c, args, "/admin/default-ingress", "ingress")
			}
			if len(args) == 0 && !clear {
				return usage("pass a node, or --clear")
			}
			if len(args) == 1 && clear {
				return usage("pass a node or --clear, not both")
			}
			p, err := a.place(c, atTile)
			if err != nil {
				return err
			}
			node := ""
			if len(args) == 1 {
				node = args[0]
			}
			v, err := a.call(PUT, p+"/ingress", map[string]string{"node": node})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			if node == "" {
				a.say("%s: public traffic enters through %s (inherited)", scopeName(p), cell(asMap(v)["node"]))
			} else {
				a.say("%s: public traffic enters through %s", scopeName(p), node)
			}
			return nil
		})
	set.Example = "  stackr ingress set home --tile admin --stack shop --env prod\n  stackr ingress set vps-fsn1 --default"
	set.Flags().BoolVar(&clear, "clear", false, "remove the pick; the scope inherits")
	set.Flags().BoolVar(&def, "default", false, "set the cluster-wide default ingress to <node>")
	scopeFlags(get, true)
	scopeFlags(set, true)
	return noun("ingress", "The node public traffic enters through", get, set)
}

func (a *app) showDefaults() error {
	v, err := a.call(GET, "/admin/node-defaults", nil)
	if err != nil {
		return err
	}
	if a.json {
		return a.show(v)
	}
	m := asMap(v)
	exit := cell(m["exit"])
	if exit == "(none)" || exit == "" {
		exit = "each node's own uplink"
	}
	a.say("default ingress\t%s\ndefault exit\t%s", cell(m["ingress"]), exit)
	return nil
}

// setDefault moves the cluster-wide default ingress or exit (admin).
func (a *app) setDefault(c *cobra.Command, args []string, path, what string) error {
	node := ""
	if len(args) > 0 {
		node = args[0]
	}
	if node == "" && what == "ingress" {
		return usage("pass the node to make the default ingress")
	}
	if len(args) > 1 {
		return usage("--default takes one node (none clears the default exit)")
	}
	v, err := a.call(PUT, path, map[string]string{"node": node})
	if err != nil {
		return err
	}
	if a.json {
		return a.show(v)
	}
	if node == "" {
		a.say("default exit cleared: every node goes out its own uplink")
	} else {
		a.say("default %s is now %s", what, node)
	}
	return nil
}

func (a *app) exits() *cobra.Command {
	var clear, def bool
	ls := leaf("ls", "org.exits,stack.exits,env.exits,tile.exits", "The exit list in force for a scope", exact(0),
		func(c *cobra.Command, _ []string) error {
			p, err := a.place(c, atTile)
			if err != nil {
				return err
			}
			v, err := a.call(GET, p+"/exits", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			m := asMap(v)
			if len(asList(m["exits"])) == 0 {
				a.say("%s: goes out through its own node (no exit list)", cell(m["scope"]))
				return nil
			}
			return a.show(m["exits"], "position", "node", "state")
		})
	ls.Example = "  stackr exit ls --stack shop --env prod"
	set := leaf("set [node]...", "org.exits-set,stack.exits-set,env.exits-set,tile.exits-set,admin.default-exit-set", "Set the ordered exit list of a scope", atLeast(0),
		func(c *cobra.Command, args []string) error {
			if def {
				return a.setDefault(c, args, "/admin/default-exit", "exit")
			}
			if len(args) == 0 && !clear {
				return usage("pass the exit nodes in order, or --clear")
			}
			if len(args) > 0 && clear {
				return usage("pass nodes or --clear, not both")
			}
			p, err := a.place(c, atTile)
			if err != nil {
				return err
			}
			v, err := a.call(PUT, p+"/exits", map[string][]string{"nodes": append([]string{}, args...)})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			switch len(args) {
			case 0:
				a.say("%s: goes out through its own node", scopeName(p))
			case 1:
				a.say("%s: goes out through %s; no internet if it is down", scopeName(p), args[0])
			default:
				a.say("%s: goes out through %s; no internet if all are down",
					scopeName(p), strings.Join(args, ", then "))
			}
			return nil
		})
	set.Example = "  stackr exit set vps-fsn1 home --stack shop --env prod\n  stackr exit set --clear --stack shop --env prod"
	set.Flags().BoolVar(&clear, "clear", false, "remove the list: the scope goes out through its own node")
	set.Flags().BoolVar(&def, "default", false, "set the cluster-wide default exit to <node> (no node clears it)")
	scopeFlags(ls, true)
	scopeFlags(set, true)
	return noun("exit", "The nodes traffic leaves the cluster through", ls, set)
}

// ---- env: node column, move, lost, restore ----

func (a *app) envLs() *cobra.Command {
	return leaf("ls", "env.list,node.list", "List environments", exact(0),
		a.at(atStack, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/envs", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			names, _ := a.nodeNames()
			for _, e := range asList(v) {
				m := asMap(e)
				m["node"] = names[cell(m["node_id"])]
				if m["node"] == "" {
					m["node"] = "(lost)"
				}
			}
			return a.show(v, "slug", "name", "type", "node", "from_kind", "from_branch", "auto", "release_id", "id")
		}))
}

func (a *app) envMove() *cobra.Command {
	var to string
	var cold, dry bool
	c := waits(leaf("move", "env.get,node.list,env.move", "Move the env to another node", exact(0),
		a.at(atEnv, func(c *cobra.Command, p string, _ []string) error {
			if to == "" {
				return usage("--to <node> is required; stackr node ls lists them")
			}
			e, err := a.call(GET, p, nil)
			if err != nil {
				return err
			}
			names, err := a.nodeNames()
			if err != nil {
				return err
			}
			from := names[cell(asMap(e)["node_id"])]
			mode := "live: copies while running, then stops for a final copy"
			if cold {
				mode = "cold: stops first and copies once; longer downtime"
			}
			q := fmt.Sprintf("Move %s from %s to %s?\n  mode   %s\n  after  new addresses on %s; routes, links and exits follow; old volumes stay on %s",
				scopeName(p), from, to, mode, to, from)
			if dry {
				a.say("%s", q)
				return nil
			}
			if err := a.confirm(q); err != nil {
				return err
			}
			return a.orgJob(c, POST, p+"/move", map[string]any{"to": to, "cold": cold}, "move")
		})))
	c.Example = "  stackr env move --stack shop --env prod --to vps-fsn1\n  stackr env move --stack shop --env prod --to home --cold"
	c.Flags().StringVar(&to, "to", "", "the node to move to")
	c.Flags().BoolVar(&cold, "cold", false, "stop first and copy once (default: live)")
	c.Flags().BoolVar(&dry, "dry-run", false, "print the plan and stop")
	return c
}

func (a *app) envLost() *cobra.Command {
	return leaf("lost", "env.lost", "List envs whose node was forgotten", exact(0), func(*cobra.Command, []string) error {
		p, err := a.orgPath()
		if err != nil {
			return err
		}
		v, err := a.call(GET, p+"/lost-envs", nil)
		if err != nil {
			return err
		}
		if a.json {
			return a.show(v)
		}
		for _, e := range asList(v) {
			m := asMap(e)
			var vols []string
			for _, x := range asList(m["volumes"]) {
				vols = append(vols, cell(asMap(x)["name"]))
			}
			m["vols"] = strings.Join(vols, ",")
		}
		return a.show(v, "path", "vols")
	})
}

func (a *app) envRestore() *cobra.Command {
	var to string
	c := leaf("restore <env>", "env.lost,node.list,env.restore,backup.restore,tile.start", "Bring a lost env back from its backups on a live node", exact(1),
		func(c *cobra.Command, args []string) error {
			if to == "" {
				return usage("--to <node> is required; stackr node ls lists them")
			}
			sp, err := a.path(c, atStack, "")
			if err != nil {
				return err
			}
			op, _ := a.orgPath()
			p := sp + "/envs/" + url.PathEscape(args[0])
			lost, err := a.call(GET, op+"/lost-envs", nil)
			if err != nil {
				return err
			}
			var env map[string]any
			for _, e := range asList(lost) {
				if m := asMap(e); cell(m["path"]) == scopeName(p) {
					env = m
				}
			}
			if env == nil {
				return fmt.Errorf("%s is not lost; stackr env lost lists the ones that are", scopeName(p))
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Restore %s on %s?", scopeName(p), to)
			for _, x := range asList(env["volumes"]) {
				vm := asMap(x)
				if t, err := time.Parse(time.RFC3339, cell(vm["last_backup"])); err == nil {
					fmt.Fprintf(&b, "\n  %s  from the backup of %s (%s)", cell(vm["name"]), t.Local().Format("2006-01-02 15:04"), ago(t))
				} else {
					fmt.Fprintf(&b, "\n  %s  no backup: starts empty", cell(vm["name"]))
				}
			}
			if err := a.confirm(b.String()); err != nil {
				return err
			}
			v, err := a.call(POST, p+"/restore", map[string]string{"to": to})
			if err != nil {
				return err
			}
			re := asMap(v)
			for _, x := range asList(asMap(re["env"])["volumes"]) {
				vm := asMap(x)
				if cell(vm["run_id"]) == "(none)" {
					continue
				}
				j, err := a.call(POST, op+"/volumes/"+cell(vm["id"])+"/restore", map[string]string{"run_id": cell(vm["run_id"]), "target_volume_id": cell(vm["id"])})
				if err != nil {
					return err
				}
				if err := a.follow(op, j, "restore of "+cell(vm["name"])); err != nil {
					return err
				}
			}
			for _, id := range strs(re["tiles"]) {
				j, err := a.call(POST, p+"/tiles/"+id+"/start", nil)
				if err != nil {
					return err
				}
				if err := a.follow(op, j, "start"); err != nil {
					return err
				}
			}
			a.say("%s: running on %s", scopeName(p), to)
			return nil
		})
	c.Example = "  stackr env restore dev --stack api --to home"
	c.Flags().StringVar(&to, "to", "", "the node to restore onto")
	return c
}

// ---- registry login ----

// dockerLogin pipes the password to `docker login`; tests replace it.
var dockerLogin = func(host, user, password string) error {
	cmd := exec.Command("docker", "login", host, "-u", user, "--password-stdin")
	cmd.Stdin = strings.NewReader(password)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func (a *app) registry() *cobra.Command {
	var host string
	login := leaf("login", "registry.login", "Let docker push and pull use the stackr registry", exact(0), func(*cobra.Command, []string) error {
		if a.server() == "" {
			return fmt.Errorf("not logged in; run stackr login <url>")
		}
		if host == "" {
			u, err := url.Parse(a.server())
			if err != nil || u.Hostname() == "" {
				return fmt.Errorf("cannot read a host from the server URL %q; pass --registry", a.server())
			}
			host = "registry." + u.Hostname()
		}
		v, err := a.call(POST, "/registry/login", nil)
		if err != nil {
			return err
		}
		m := asMap(v)
		if err := dockerLogin(host, "stackr", cell(m["token"])); err != nil {
			return fmt.Errorf("docker login %s failed: %w", host, err)
		}
		a.say("Logged docker in to %s. It works until %s; run this again after.", host, cell(m["expires_at"]))
		return nil
	})
	login.Example = "  stackr registry login\n  docker push registry.example.com/acme/shop/web:1.4"
	login.Flags().StringVar(&host, "registry", "", "the registry host (default: registry. plus the server's host)")
	return noun("registry", "The stackr image registry", append([]*cobra.Command{login}, a.registryImages()...)...)
}
