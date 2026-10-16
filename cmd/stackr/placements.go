package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Placements: a named place envs belong to, standing on one node. Members
// list the ones open to their org (never a node); `stackr admin placement`
// is the server admin's side, where the node shows.

// placements is the dev list: what the org may pin to.
func (a *app) placements() *cobra.Command {
	c := leaf("placements", "org.placements", "List the placements open to the org", exact(0),
		func(*cobra.Command, []string) error {
			p, err := a.orgPath()
			if err != nil {
				return err
			}
			v, err := a.call(GET, p+"/placements", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			for _, x := range asList(v) {
				m := asMap(x)
				m["tags"] = strings.Join(strs(m["tags"]), ",")
				m["public"] = yesNo(m["public"])
				m["online"] = yesNo(m["online"])
				m["usable"] = yesNo(m["usable"])
				if m["why"] == nil {
					m["why"] = ""
				}
			}
			return a.show(v, "name", "tags", "public", "online", "usable", "why")
		})
	c.Example = "  stackr placements\n  stackr placements --org acme"
	c.Flags().String("org", "", "the org (default: the logged-in one)")
	c.PreRunE = func(c *cobra.Command, _ []string) error {
		if o := flag(c, "org"); o != "" {
			a.orgSlug = o
		}
		return nil
	}
	return c
}

func yesNo(v any) string {
	if b, _ := v.(bool); b {
		return "yes"
	}
	return "no"
}

// pl reads the placement name of a pin, ingress or exit answer.
func pl(m map[string]any) string { return cell(m["placement"]) }

// adminPlacement is `stackr admin placement`.
func (a *app) adminPlacement() *cobra.Command {
	return noun("placement", "Placements: named places envs belong to, each on one node (server admin)",
		a.placementLs(), a.placementAdd(), a.placementOpen(), a.placementRm(), a.placementMove(), a.placementRetry())
}

func placementPath(name string) string { return "/admin/placements/" + url.PathEscape(name) }

// state is one line saying how a placement stands.
func placementState(m map[string]any) string {
	if mv := asMap(m["move"]); mv != nil && cell(mv["state"]) == "running" {
		return fmt.Sprintf("moving %s to %s (%v of %v envs)", cell(mv["from"]), cell(mv["to"]), mv["done"], mv["all"])
	} else if mv != nil && cell(mv["state"]) == "failed" {
		return "move failed: stackr admin placement retry"
	}
	switch {
	case m["node_forgotten"] == true:
		return "node forgotten: repoint to restore"
	case cell(m["node"]) == "(none)":
		return "no node yet"
	case m["online"] != true:
		return "offline"
	}
	return "online"
}

// stopNote says the envs of a move stop while they move, in the right number.
func stopNote(n int) string {
	if n == 1 {
		return "Its env stops while it moves."
	}
	return fmt.Sprintf("Its %d envs stop while they move.", n)
}

func (a *app) placementLs() *cobra.Command {
	return leaf("ls", "admin.placements", "List placements with their node (server admin)", exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/admin/placements", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			for _, x := range asList(v) {
				m := asMap(x)
				m["tags"] = strings.Join(strs(m["tags"]), ",")
				m["open"] = "all"
				if m["open_to_all"] != true {
					m["open"] = orList(strs(m["open_to"]))
				}
				m["state"] = placementState(m)
			}
			return a.show(v, "name", "node", "tags", "open", "envs", "state")
		})
}

// openFlag reads --open: all, or a comma list of org slugs.
func openFlag(s string) (all bool, orgs []string, err error) {
	if s == "" || s == "all" {
		return true, nil, nil
	}
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimSpace(o); o != "" {
			orgs = append(orgs, o)
		}
	}
	if len(orgs) == 0 {
		return false, nil, usage("--open is all, or org slugs separated by commas")
	}
	return false, orgs, nil
}

func (a *app) placementAdd() *cobra.Command {
	var node, open string
	c := leaf("add <name>", "admin.placement-create", "Make a placement, on a node or none yet (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			_, orgs, err := openFlag(open)
			if err != nil {
				return err
			}
			v, err := a.call(POST, "/admin/placements", map[string]any{"name": args[0], "node": node, "orgs": orgs})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			where := "no node yet; its envs wait"
			if node != "" {
				where = "on " + node
			}
			a.say("%s: made, %s", args[0], where)
			return nil
		})
	c.Example = "  stackr admin placement add eu-prod --node h4\n  stackr admin placement add eu-db --node h3 --open acme,shop"
	c.Flags().StringVar(&node, "node", "", "the node it stands on (default: none yet)")
	c.Flags().StringVar(&open, "open", "all", "all, or the org slugs that may use it, separated by commas")
	return c
}

func (a *app) placementOpen() *cobra.Command {
	return leaf("open <name> <all|org,org,...>", "admin.placement-open", "Open a placement to every org or to a list; envs already there stay (server admin)", exact(2),
		func(_ *cobra.Command, args []string) error {
			all, orgs, err := openFlag(args[1])
			if err != nil {
				return err
			}
			if _, err := a.call(PUT, placementPath(args[0])+"/open", map[string]any{"all": all, "orgs": orgs}); err != nil {
				return err
			}
			if all {
				a.say("%s: open to all orgs", args[0])
			} else {
				a.say("%s: open to %s; envs already there stay", args[0], strings.Join(orgs, ", "))
			}
			return nil
		})
}

func (a *app) placementRm() *cobra.Command {
	return leaf("rm <name>", "admin.placement-delete", "Delete a placement no pin or env uses (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			if err := a.confirm("Delete placement " + args[0] + "?"); err != nil {
				return err
			}
			if _, err := a.call(DELETE, placementPath(args[0]), nil); err != nil {
				return err
			}
			a.say("%s: deleted", args[0])
			return nil
		})
}

// sayPlan prints a move plan: the envs it takes and the checks, one line each.
func (a *app) sayPlan(m map[string]any) {
	verb := "Move"
	if m["restore"] == true {
		verb = "Restore from backups"
	}
	a.say("%s %s: %s to %s", verb, cell(m["placement"]), cell(m["from"]), cell(m["to"]))
	for _, e := range asList(m["envs"]) {
		em := asMap(e)
		data := plural(int(num(em["volumes"])), "volume", "volumes")
		if b := num(em["bytes"]); b >= 0 && num(em["volumes"]) > 0 {
			data += ", " + gb(b)
		}
		a.say("  env   %s\t%s\tdown %s", cell(em["path"]), data, cell(em["downtime"]))
	}
	for _, ck := range asList(m["checks"]) {
		cm := asMap(ck)
		mark, tail := "ok  ", ""
		if cm["ok"] != true {
			mark = "FAIL"
		}
		if d := cell(cm["detail"]); d != "(none)" {
			tail = "  " + d
		}
		a.say("  %s  %s%s", mark, cell(cm["name"]), tail)
	}
}

func (a *app) placementMove() *cobra.Command {
	c := waits(leaf("move <name> <node>", "admin.placement-move-plan,admin.placement-move,admin.placement-move-status", "Repoint a placement to another node, moving its envs (server admin)", exact(2),
		func(c *cobra.Command, args []string) error {
			name, to := args[0], args[1]
			plan, err := a.call(GET, placementPath(name)+"/move-plan?to="+url.QueryEscape(to), nil)
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
				return fmt.Errorf("%s cannot move to %s yet: a check failed", name, to)
			}
			q := fmt.Sprintf("Move placement %s from %s to %s? %s", name, cell(pm["from"]), to, stopNote(len(asList(pm["envs"]))))
			if err := a.confirm(q); err != nil {
				return err
			}
			// --yes is the typed name: the server wants it back as confirm.
			return a.followMove(c, name, placementPath(name)+"/move", map[string]string{"to": to, "confirm": name})
		}))
	c.Example = "  stackr admin placement move eu-prod h5\n  stackr admin placement move eu-prod h5 --yes"
	return c
}

func (a *app) placementRetry() *cobra.Command {
	return waits(leaf("retry <name>", "admin.placement-retry,admin.placement-move-status", "Move the envs a failed repoint left behind (server admin)", exact(1),
		func(c *cobra.Command, args []string) error {
			if st, err := a.call(GET, placementPath(args[0])+"/move", nil); err == nil && !a.json {
				m := asMap(st)
				a.say("%s: %s to %s, %v of %v envs moved", args[0], cell(m["state"]), cell(m["to"]), m["done"], m["all"])
				if e := cell(m["error"]); e != "(none)" {
					a.say("  %s", e)
				}
				a.sayMoveEnvs(m, map[string]string{})
			}
			return a.followMove(c, args[0], placementPath(args[0])+"/retry", nil)
		}))
}

// sayMoveEnvs prints a repoint's env lines, "  acme/shop/prod  moving: Copy
// while running", each when it changed since said last held it.
func (a *app) sayMoveEnvs(m map[string]any, said map[string]string) {
	for _, x := range asList(m["envs"]) {
		e := asMap(x)
		line := "moved"
		switch {
		case e["active"] == true:
			line = "moving"
			for _, st := range asList(e["steps"]) {
				if sm := asMap(st); cell(sm["state"]) == "running" {
					line += ": " + cell(sm["label"])
				}
			}
		case cell(e["error"]) != "(none)":
			line = "failed: " + cell(e["error"])
		}
		path := cell(e["path"])
		if said[path] != line {
			said[path] = line
			a.say("  %s\t%s", path, line)
		}
	}
}

// followMove starts a repoint or its retry and follows it to the end: the
// job it returns is the first env's, so after that the repoint's own state
// is polled until it is no longer running.
func (a *app) followMove(c *cobra.Command, name, path string, body any) error {
	if err := a.job(c, "/admin", POST, path, body, "move of "+name); err != nil {
		return err
	}
	if nw, _ := c.Flags().GetBool("no-wait"); nw {
		return nil
	}
	said := map[string]string{}
	for {
		st, err := a.call(GET, placementPath(name)+"/move", nil)
		if err != nil {
			return err
		}
		m := asMap(st)
		if !a.json {
			a.sayMoveEnvs(m, said)
		}
		if cell(m["state"]) == "running" {
			time.Sleep(2 * time.Second)
			continue
		}
		if !a.json {
			a.say("%s: %s to %s, %v of %v envs moved", name, cell(m["state"]), cell(m["to"]), m["done"], m["all"])
		}
		if cell(m["state"]) == "failed" {
			return fmt.Errorf("the move of %s failed: %s; fix it, then stackr admin placement retry %s", name, cell(m["error"]), name)
		}
		return nil
	}
}
