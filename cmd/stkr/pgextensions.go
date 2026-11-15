package main

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var pgRecipeCols = []string{"name", "builtin", "enabled", "version", "majors", "extensions", "share"}

// serverPGExtensions is `stkr server pg-extensions`: the postgres extension
// recipes the server offers (admin).
func (a *app) serverPGExtensions() *cobra.Command {
	var on, off, all bool
	var orgs, pkgs, majors, exts, preload, requires, sha []string
	var version, url string
	ls := leaf("list", "admin.pg-extension-list", "List the built-in and added postgres extension recipes", exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/admin/pg-extensions", nil)
			if err != nil {
				return err
			}
			return a.show(v, pgRecipeCols...)
		})
	set := leaf("set <name>", "admin.pg-extension-set",
		"Add or rewrite a recipe, or switch a built-in on or off (--off); --org or --all say who may use it", exact(1),
		func(c *cobra.Command, args []string) error {
			if on && off {
				return usage("--on and --off exclude each other")
			}
			if all && len(orgs) > 0 {
				return usage("--all shares with every organization; drop --org")
			}
			body := map[string]any{"enabled": !off, "share": append([]string{}, orgs...)}
			if all {
				body["share"] = []string{"all"}
			}
			fl := c.Flags()
			if fl.Changed("package") {
				body["packages"] = append([]string{}, pkgs...)
			}
			if fl.Changed("major") {
				ms := []int{}
				for _, m := range majors {
					n, err := strconv.Atoi(m)
					if err != nil {
						return usage("--major is a whole number")
					}
					ms = append(ms, n)
				}
				body["majors"] = ms
			}
			if fl.Changed("sha256") {
				m := map[string]string{}
				for _, s := range sha {
					k, v, ok := strings.Cut(s, "=")
					if !ok || k == "" || v == "" {
						return usage("--sha256 is arch=hex, for example amd64=ab12")
					}
					m[k] = v
				}
				body["sha256"] = m
			}
			if fl.Changed("version") {
				body["version"] = version
			}
			if fl.Changed("package-url") {
				body["package_url"] = url
			}
			if fl.Changed("extension") {
				body["extensions"] = append([]string{}, exts...)
			}
			if fl.Changed("preload") {
				body["preload"] = append([]string{}, preload...)
			}
			if fl.Changed("requires") {
				body["requires"] = append([]string{}, requires...)
			}
			v, err := a.call(PUT, "/admin/pg-extensions/"+args[0], body)
			if err != nil {
				return err
			}
			return a.show(v, pgRecipeCols...)
		})
	f := set.Flags()
	f.BoolVar(&on, "on", false, "switch the recipe on (the default)")
	f.BoolVar(&off, "off", false, "switch the recipe off")
	f.StringArrayVar(&orgs, "org", nil, "an organization id that may use it (repeatable)")
	f.BoolVar(&all, "all", false, "every organization may use it")
	f.StringSliceVar(&pkgs, "package", nil, "apt package names, with {major} where the postgres major goes")
	f.StringSliceVar(&majors, "major", nil, "postgres majors it supports")
	f.StringVar(&url, "package-url", "", "download url of a package file")
	f.StringArrayVar(&sha, "sha256", nil, "arch=hex checksum of the package file (repeatable)")
	f.StringVar(&version, "version", "", "the version to pin")
	f.StringSliceVar(&exts, "extension", nil, "names CREATE EXTENSION creates")
	f.StringSliceVar(&preload, "preload", nil, "startup modules it needs")
	f.StringSliceVar(&requires, "requires", nil, "recipes it needs first")
	rm := leaf("rm <name>", "admin.pg-extension-delete", "Remove an added recipe, or a built-in's switch-off row", exact(1),
		func(_ *cobra.Command, args []string) error {
			if err := a.confirm("Remove recipe " + args[0] + "? Instances that use it keep their image."); err != nil {
				return err
			}
			_, err := a.call(DELETE, "/admin/pg-extensions/"+args[0], nil)
			return err
		})
	return noun("pg-extensions", "Postgres extension recipes the server offers (admin)", ls, set, rm)
}

// operatedExtensionsPlan is `stkr operated extensions plan`: queue the plan
// of a recipe change, wait for it, print it with its id.
func (a *app) operatedExtensionsPlan() *cobra.Command {
	c := waits(leaf("plan <tile> [name...]", "operated.extensions-plan,operated.extensions-plan-get",
		"(postgres) Plan an extension change that needs a build; prints the plan and its id for apply", atLeast(1), nil))
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, args []string) error {
		base, err := a.orgPath()
		if err != nil {
			return err
		}
		body := map[string]any{"extensions": append([]string{}, args[1:]...)}
		if err := a.job(c, base, POST, p+"/extensions/plan", body, "plan"); err != nil {
			return err
		}
		if nw, _ := c.Flags().GetBool("no-wait"); nw {
			return nil
		}
		v, err := a.call(GET, p+"/extensions/plan", nil)
		if err != nil {
			return err
		}
		m, _ := v.(map[string]any)
		if a.json || m == nil {
			return a.show(v)
		}
		for _, l := range asStrings(m["lines"]) {
			a.say("%s", l)
		}
		for _, b := range asStrings(m["blockers"]) {
			a.say("Blocked: %s", b)
		}
		a.say("Plan id: %s", cell(m["id"]))
		return nil
	})
	return c
}

func asStrings(v any) []string {
	var out []string
	xs, _ := v.([]any)
	for _, x := range xs {
		out = append(out, cell(x))
	}
	return out
}

// operatedExtensionsApply is `stkr operated extensions apply`: apply the
// reviewed plan by id. A plan that restarts the instance is confirmed first.
func (a *app) operatedExtensionsApply() *cobra.Command {
	var plan string
	c := waits(leaf("apply <tile>", "operated.extensions-apply,operated.extensions-plan-get",
		"(postgres) Apply a reviewed extension plan; --yes confirms a plan that restarts the instance", exact(1), nil))
	c.Flags().StringVar(&plan, "plan", "", "the plan id that plan printed")
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if plan == "" {
			return usage("--plan is the id that stkr operated extensions plan printed")
		}
		confirm := a.yes
		stored, err := a.call(GET, p+"/extensions/plan", nil)
		if err != nil {
			return err
		}
		if m, _ := stored.(map[string]any); m != nil && m["restart"] == true && cell(m["id"]) == plan {
			if err := a.confirm("This restarts the instance. Apply?"); err != nil {
				return err
			}
			confirm = true
		}
		v, err := a.call(POST, p+"/extensions/apply", map[string]any{"plan": plan, "confirm": confirm})
		if err != nil {
			return err
		}
		m, _ := v.(map[string]any)
		if m["job"] == nil {
			return a.show(v)
		}
		if nw, _ := c.Flags().GetBool("no-wait"); nw {
			return a.show(m["job"], "id", "kind", "state")
		}
		base, err := a.orgPath()
		if err != nil {
			return err
		}
		return a.follow(base, m["job"], "extensions")
	})
	return c
}
