package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// Placement: pin, ingress and exit work the same at every scope, so they are
// top-level nouns with --org, --stack, --env and --tile; they name placements,
// never nodes. The lost-env verbs are under env (stack.go adds them).

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
	c := waits(leaf("pin [placement]", "org.pin,org.pin-set,org.pin-plan,stack.pin,stack.pin-set,stack.pin-plan,env.pin,env.pin-set,env.pin-plan", "Pin an org, stack or env to a placement: where its envs run", upTo(1),
		func(c *cobra.Command, args []string) error {
			p, err := a.place(c, atEnv)
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
				if pl(m) == "(none)" {
					a.say("%s: no pin; new envs go to the default placement", cell(m["scope"]))
				} else {
					a.say("%s: envs go to %s", cell(m["scope"]), pl(m))
				}
				return nil
			}
			if len(args) == 1 && clear {
				return usage("pass a placement or --clear, not both")
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			body := map[string]string{"placement": name}
			// A pin that moves envs to another node shows each one's plan
			// first and needs --yes; the confirm is the scope's slug.
			plans, err := a.pinPlans(p, name)
			if err != nil {
				return err
			}
			if len(plans) > 0 {
				var paths []string
				for _, pl := range plans {
					pm := asMap(pl)
					if !a.json {
						a.sayPlan(pm)
					}
					if pm["ok"] != true {
						return fmt.Errorf("%s cannot move to %s yet: a check failed", cell(pm["placement"]), cell(pm["to"]))
					}
					paths = append(paths, cell(pm["placement"]))
				}
				if a.json {
					if err := a.show(plans); err != nil {
						return err
					}
				}
				to := name
				if to == "" {
					to = cell(asMap(plans[0])["to"]) + " (inherited)"
				}
				q := fmt.Sprintf("Pin %s to %s? That moves %s (%s); each stops while it moves.",
					scopeName(p), to, plural(len(paths), "env", "envs"), strings.Join(paths, ", "))
				if err := a.confirm(q); err != nil {
					return err
				}
				body["confirm"] = last(p)
			}
			v, err := a.call(PUT, p+"/pin", body)
			if err != nil {
				return err
			}
			m := asMap(v)
			jobs := asList(m["jobs"])
			if j := m["job"]; j != nil { // an env pin moved the env
				jobs = []any{j}
			}
			if len(jobs) > 0 {
				if nw, _ := c.Flags().GetBool("no-wait"); nw {
					return a.show(jobs, "id", "kind", "state")
				}
				base, err := a.orgPath()
				if err != nil {
					return err
				}
				for _, j := range jobs {
					if err := a.follow(base, j, "move"); err != nil {
						return err
					}
				}
				return nil
			}
			if a.json {
				return a.show(v)
			}
			if name == "" {
				a.say("%s: pin removed; envs follow the pin above, else the default placement", scopeName(p))
			} else {
				a.say("%s: envs go to %s", scopeName(p), name)
			}
			return nil
		}))
	c.Long = "Pin an org, stack or env to a placement; stackr placements lists the ones open to the org.\n" +
		"Pinning changes where existing envs run: each one on another node moves, after its plan is shown and --yes."
	c.Example = "  stackr pin eu-prod --org acme\n  stackr pin eu-prod --stack shop\n  stackr pin eu-db --stack shop --env prod --yes\n  stackr pin --clear --stack shop"
	c.Flags().BoolVar(&clear, "clear", false, "remove the pin")
	scopeFlags(c, false)
	return c
}

// pinPlans is the move plans pinning the scope at p to name would cause:
// an env's one plan when it moves, an org's or stack's one per env.
func (a *app) pinPlans(p, name string) ([]any, error) {
	plan, err := a.call(GET, p+"/pin-plan?to="+url.QueryEscape(name), nil)
	if err != nil || !strings.Contains(p, "/envs/") {
		return asList(plan), err
	}
	if len(asList(asMap(plan)["envs"])) == 0 {
		return nil, nil
	}
	return []any{plan}, nil
}

func (a *app) ingress() *cobra.Command {
	var clear, def bool
	var getDef bool
	var confirm string
	get := leaf("get", "org.ingress,stack.ingress,env.ingress,tile.ingress,admin.node-defaults", "Which placement public traffic enters through", exact(0),
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
			a.say("%s\t%s\t(from %s)", cell(m["scope"]), pl(m), cell(m["from"]))
			return nil
		})
	get.Example = "  stackr ingress get --stack shop --env prod\n  stackr ingress get --default"
	get.Flags().BoolVar(&getDef, "default", false, "show the cluster-wide default ingress and exit (server admin)")
	set := leaf("set [placement]", "org.ingress-set,stack.ingress-set,env.ingress-set,tile.ingress-set,admin.default-ingress-set,admin.default-ingress-plan", "Pick the ingress placement of a scope", upTo(1),
		func(c *cobra.Command, args []string) error {
			if def {
				return a.setDefaultIngress(args, confirm)
			}
			if len(args) == 0 && !clear {
				return usage("pass a placement, or --clear")
			}
			if len(args) == 1 && clear {
				return usage("pass a placement or --clear, not both")
			}
			p, err := a.place(c, atTile)
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			v, err := a.call(PUT, p+"/ingress", map[string]string{"placement": name})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			if name == "" {
				a.say("%s: public traffic enters through %s (inherited)", scopeName(p), pl(asMap(v)))
			} else {
				a.say("%s: public traffic enters through %s", scopeName(p), name)
			}
			return nil
		})
	set.Example = "  stackr ingress set eu-prod --tile admin --stack shop --env prod\n  stackr ingress set h4 --default"
	set.Flags().BoolVar(&clear, "clear", false, "remove the pick; the scope inherits")
	set.Flags().BoolVar(&def, "default", false, "set the cluster-wide default ingress to <node> (server admin); shows where the hosts point first")
	set.Flags().StringVar(&confirm, "confirm", "", "with --default: the node's name, to skip the prompt")
	scopeFlags(get, true)
	scopeFlags(set, true)
	return noun("ingress", "The placement public traffic enters through", get, set)
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

// setDefaultIngress shows where the hosts that enter by the default point,
// then moves the default once the node's name is typed (admin).
func (a *app) setDefaultIngress(args []string, confirm string) error {
	if len(args) != 1 {
		return usage("pass the node to make the default ingress")
	}
	node := args[0]
	plan, err := a.call(GET, "/admin/default-ingress/plan?node="+url.QueryEscape(node), nil)
	if err != nil {
		return err
	}
	if !a.json {
		a.say("The panel and every route without an ingress pick move to %s.", node)
		for _, h := range asList(asMap(plan)["hosts"]) {
			m := asMap(h)
			where := "points to " + strings.Join(strs(m["points"]), ", ")
			switch {
			case m["ok"] == true:
				where = "points here"
			case cell(m["error"]) != "" && cell(m["error"]) != "(none)":
				where = "lookup failed: " + cell(m["error"])
			}
			kind := "route"
			if m["panel"] == true {
				kind = "panel"
			}
			a.say("  %s\t%s\t%s", kind, cell(m["host"]), where)
		}
		if asMap(plan)["known"] != true {
			a.say("%s has no public address of its own; check the DNS by hand.", node)
		}
	}
	if confirm == "" {
		if a.json || !a.tty {
			return usage("pass --confirm %s to switch without a prompt", node)
		}
		_, _ = fmt.Fprintf(a.errw, "Type %s to confirm: ", node)
		line, _ := a.in.ReadString('\n')
		confirm = strings.TrimSpace(line)
	}
	v, err := a.call(PUT, "/admin/default-ingress", map[string]string{"node": node, "confirm": confirm})
	if err != nil {
		return err
	}
	if a.json {
		return a.show(v)
	}
	a.say("default ingress is now %s", node)
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
				a.say("%s: goes out through its own placement (no exit list)", cell(m["scope"]))
				return nil
			}
			return a.show(m["exits"], "position", "placement", "state")
		})
	ls.Example = "  stackr exit ls --stack shop --env prod"
	set := leaf("set [placement]...", "org.exits-set,stack.exits-set,env.exits-set,tile.exits-set,admin.default-exit-set", "Set the ordered exit list of a scope: placements, or own for the env's own", atLeast(0),
		func(c *cobra.Command, args []string) error {
			if def {
				return a.setDefault(c, args, "/admin/default-exit", "exit")
			}
			if len(args) == 0 && !clear {
				return usage("pass the exit placements in order (or own), or --clear")
			}
			if len(args) > 0 && clear {
				return usage("pass placements or --clear, not both")
			}
			p, err := a.place(c, atTile)
			if err != nil {
				return err
			}
			v, err := a.call(PUT, p+"/exits", map[string][]string{"placements": append([]string{}, args...)})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			switch len(args) {
			case 0:
				a.say("%s: goes out through its own placement", scopeName(p))
			case 1:
				a.say("%s: goes out through %s; no internet if it is down", scopeName(p), args[0])
			default:
				a.say("%s: goes out through %s; no internet if all are down",
					scopeName(p), strings.Join(args, ", then "))
			}
			return nil
		})
	set.Example = "  stackr exit set eu-prod home --stack shop --env prod\n  stackr exit set --clear --stack shop --env prod"
	set.Flags().BoolVar(&clear, "clear", false, "remove the list: the scope goes out through its own node")
	set.Flags().BoolVar(&def, "default", false, "set the cluster-wide default exit to <node> (server admin; no node clears it)")
	scopeFlags(ls, true)
	scopeFlags(set, true)
	return noun("exit", "The placements traffic leaves the cluster through", ls, set)
}

// ---- env: node column, move, lost, restore ----

func (a *app) envLs() *cobra.Command {
	return leaf("ls", "env.list", "List environments", exact(0),
		a.at(atStack, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/envs", nil)
			if err != nil {
				return err
			}
			return a.show(v, "slug", "name", "type", "placement", "from_kind", "from_branch", "auto", "release_id", "id")
		}))
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
		return a.show(v, "path", "placement", "vols")
	})
}

// envRestore brings lost envs back by repointing their placement: the
// server rehomes each env and restores its volumes from the backups.
func (a *app) envRestore() *cobra.Command {
	var placement, to string
	c := waits(leaf("restore", "admin.placement-move-plan,admin.placement-move,admin.placement-move-status", "Bring a placement's lost envs back from their backups on a live node (server admin)", exact(0),
		func(c *cobra.Command, _ []string) error {
			if placement == "" || to == "" {
				return usage("--placement <name> and --to <node> are required; stackr env lost lists the lost envs")
			}
			plan, err := a.call(GET, placementPath(placement)+"/move-plan?to="+url.QueryEscape(to), nil)
			if err != nil {
				return err
			}
			pm := asMap(plan)
			if a.json {
				if err := a.show(plan); err != nil {
					return err
				}
			} else {
				a.sayPlan(pm)
			}
			if pm["ok"] != true {
				return fmt.Errorf("%s cannot be restored on %s yet: a check failed", placement, to)
			}
			if pm["restore"] != true {
				return fmt.Errorf("%s is not on a forgotten node; stackr admin placement move repoints it", placement)
			}
			if err := a.confirm(fmt.Sprintf("Restore placement %s on %s? Each env is brought back from its last backup; data written since is lost.", placement, to)); err != nil {
				return err
			}
			return a.followMove(c, placement, placementPath(placement)+"/move", map[string]string{"to": to, "confirm": placement})
		}))
	c.Example = "  stackr env restore --placement eu-db --to h5"
	c.Flags().StringVar(&placement, "placement", "", "the placement whose node was forgotten")
	c.Flags().StringVar(&to, "to", "", "the live node to restore onto")
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
