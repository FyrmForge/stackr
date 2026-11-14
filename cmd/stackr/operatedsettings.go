package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// operatedSettings returns the `settings`, `version`, `update`, `upgrade` and `rollback-upgrade` verbs of `stackr operated`: a postgres instance's own settings,
// mirroring the API. The instance is the first argument.
func (a *app) operatedSettings() []*cobra.Command {
	list := leaf("get <tile>", "operated.settings", "Show an instance's settings and where each value comes from", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/settings", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if a.json || m == nil {
				return a.show(v)
			}
			if err := a.show(m["settings"], "name", "value", "source", "needs_restart"); err != nil {
				return err
			}
			if m["running"] != true {
				a.say("The instance is not running; values it reports are missing.")
			}
			if m["work_mem_risk"] == true {
				a.say("work_mem times max_connections is more than the node's memory.")
			}
			return nil
		}))
	put := func(body map[string]any) func(c *cobra.Command, p string, _ []string) error {
		return func(c *cobra.Command, p string, _ []string) error {
			v, err := a.call(PUT, p+"/settings", body)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if m["job"] != nil {
				base, err := a.orgPath()
				if err != nil {
					return err
				}
				if nw, _ := c.Flags().GetBool("no-wait"); nw {
					return a.show(m["job"], "id", "kind", "state")
				}
				return a.follow(base, m["job"], "restart")
			}
			if a.json {
				return a.show(v)
			}
			a.say("Applied without a restart.")
			return nil
		}
	}
	set := leaf("set <tile> <name=value>...", "operated.settings-set",
		"Set settings; a reload applies them, or the instance restarts when one needs it", atLeast(2), nil)
	set.RunE = func(c *cobra.Command, args []string) error {
		kv := map[string]string{}
		for _, s := range args[1:] {
			k, v, ok := strings.Cut(s, "=")
			if !ok || k == "" || v == "" {
				return usage("%q is not name=value", s)
			}
			kv[k] = v
		}
		return a.at(atTile, put(map[string]any{"set": kv}))(c, args)
	}
	unset := leaf("unset <tile> <name>...", "operated.settings-set",
		"Remove your override of settings, back to the computed or postgres default", atLeast(2), nil)
	unset.RunE = func(c *cobra.Command, args []string) error {
		return a.at(atTile, put(map[string]any{"unset": args[1:]}))(c, args)
	}
	version := leaf("version <tile>", "operated.version", "(postgres) Show the version an instance runs and whether an update is out", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
			v, err := a.call(GET, p+"/version", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if a.json || m == nil {
				return a.show(v)
			}
			a.say("%s (postgres %v)", cell(m["image"]), m["major"])
			if to := cell(m["update_to"]); to != "" {
				a.say("Update available: %s. Run `stackr operated update %s`.", to, args[0])
			}
			if m["eol"] == true {
				a.say("Postgres %v no longer gets security fixes (ended %s).", m["major"], cell(m["eol_date"]))
			}
			if rb, _ := m["rollback"].(map[string]any); rb != nil {
				a.say("The old data of postgres %v is kept until %s. Run `stackr operated rollback-upgrade %s` to return to it.", rb["from"], cell(rb["until"]), args[0])
			}
			return nil
		}))
	update := waits(leaf("update <tile>", "operated.update", "(postgres) Move an instance to the current minor of its postgres major; it restarts", exact(1),
		a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
			v, err := a.call(POST, p+"/version", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if m["job"] == nil {
				a.say("Image updated; it applies on the next deploy.")
				return nil
			}
			if nw, _ := c.Flags().GetBool("no-wait"); nw {
				return a.show(m["job"], "id", "kind", "state")
			}
			base, err := a.orgPath()
			if err != nil {
				return err
			}
			return a.follow(base, m["job"], "update")
		})))
	return []*cobra.Command{
		noun("settings", "(postgres) An instance's settings", list, waits(set), waits(unset)), version, update,
		a.operatedUpgrade(), a.operatedRollbackUpgrade(), a.operatedExtensions(),
	}
}

// operatedUpgrade is `stackr operated upgrade`: move an instance to another postgres
// major, up or down. The data goes into a new volume; the old one is kept for
// a week. --check only asks whether it can, and changes nothing.
func (a *app) operatedUpgrade() *cobra.Command {
	var to int
	var check bool
	c := waits(leaf("upgrade <tile>", "operated.upgrade",
		"(postgres) Move an instance to another postgres major (down too); the old data is kept for 7 days. --check only asks whether it can", exact(1), nil))
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if to == 0 {
			return usage("pass --to: the postgres major to move to, such as 18")
		}
		if !check {
			if err := a.confirm(fmt.Sprintf("Move this instance to postgres %d? It is stopped while the data is converted. The old data is kept for 7 days.", to)); err != nil {
				return err
			}
		}
		base, err := a.orgPath()
		if err != nil {
			return err
		}
		if err := a.job(c, base, POST, p+"/upgrade", map[string]any{"to": to, "check": check}, "upgrade"); err != nil || !check {
			return err
		}
		v, err := a.call(GET, p+"/version", nil)
		if err != nil {
			return err
		}
		if m, _ := v.(map[string]any); m != nil {
			if ck, _ := m["check"].(map[string]any); ck != nil {
				a.say("%s", cell(ck["detail"]))
			}
		}
		return nil
	})
	c.Flags().IntVar(&to, "to", 0, "the postgres major to move to")
	c.Flags().BoolVar(&check, "check", false, "only check whether the data can move; change nothing")
	return c
}

// dbRollbackUpgrade is `stackr operated rollback-upgrade`: back to the major the
// instance ran before its last change, on the data it had then.
func (a *app) operatedRollbackUpgrade() *cobra.Command {
	c := waits(leaf("rollback-upgrade <tile>", "operated.rollback-upgrade",
		"(postgres) Return to the postgres major before the last change, with the data it had then; what was written since is lost", exact(1), nil))
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if err := a.confirm("Roll back? Everything written since the version change is lost."); err != nil {
			return err
		}
		base, err := a.orgPath()
		if err != nil {
			return err
		}
		return a.job(c, base, POST, p+"/rollback-upgrade", nil, "rollback")
	})
	return c
}

// operatedExtensions is `stackr operated extensions`: an instance's image,
// startup modules and extensions. set follows the job a change queues.
func (a *app) operatedExtensions() *cobra.Command {
	get := leaf("get <tile>", "operated.extensions", "(postgres) Show an instance's image, startup modules and extensions", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/extensions", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if a.json || m == nil {
				return a.show(v)
			}
			a.say("%s", cell(m["image"]))
			if s := cell(m["busy"]); s != "" {
				a.say("A change is in progress: %s.", s)
			}
			if s := cell(m["plan"]); s != "" {
				a.say("%s", s)
			}
			if u, _ := m["updates"].(map[string]any); u != nil {
				a.say("Extension updates:")
				cs, _ := u["changes"].([]any)
				for _, c := range cs {
					c, _ := c.(map[string]any)
					a.say("  %s %s to %s", cell(c["package"]), cell(c["from"]), cell(c["to"]))
				}
			}
			return a.show(m["items"], "name", "installed", "available", "listed", "startup", "used_by")
		}))
	var image string
	var preload []string
	set := waits(leaf("set <tile> [name...]", "operated.extensions-set",
		"(postgres) Set an instance's extensions, startup modules or image; with no names and no flags the list is emptied", atLeast(1), nil))
	set.Flags().StringVar(&image, "image", "", "the image ref")
	set.Flags().StringSliceVar(&preload, "preload", nil, "startup modules, comma separated; \"\" empties the list")
	set.RunE = a.at(atTile, func(c *cobra.Command, p string, args []string) error {
		body := map[string]any{}
		if c.Flags().Changed("image") {
			body["image"] = image
		}
		if c.Flags().Changed("preload") {
			body["preload"] = append([]string{}, preload...)
		}
		if len(args) > 1 || len(body) == 0 {
			body["extensions"] = append([]string{}, args[1:]...)
		}
		v, err := a.call(PUT, p+"/extensions", body)
		if err != nil {
			return err
		}
		m, _ := v.(map[string]any)
		if m["job"] == nil {
			if a.json {
				return a.show(v)
			}
			a.say("Saved. Applies on the next deploy.")
			return nil
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
	return noun("extensions", "(postgres) An instance's extensions", get, set, a.operatedExtensionsPlan(), a.operatedExtensionsApply())
}

// sliceExtensions is `stackr slice extensions`: the extensions of a slice's
// database. set follows the slice's deploy when one comes back.
func (a *app) sliceExtensions() *cobra.Command {
	get := leaf("get <slice>", "slice.extensions", "Show a slice's extensions against the ones its instance allows", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/slice-extensions", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if a.json || m == nil {
				return a.show(v)
			}
			return a.show(m["items"], "name", "installed", "available", "added_with", "listed")
		}))
	set := waits(leaf("set <slice> [name...]", "slice.extensions-set",
		"Set a slice's extensions; with no names the list is emptied", atLeast(1), nil))
	set.RunE = a.at(atTile, func(c *cobra.Command, p string, args []string) error {
		v, err := a.call(PUT, p+"/slice-extensions", map[string]any{"extensions": append([]string{}, args[1:]...)})
		if err != nil {
			return err
		}
		m, _ := v.(map[string]any)
		if m["job"] == nil {
			if a.json {
				return a.show(v)
			}
			a.say("Saved. Applies on the next deploy.")
			return nil
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
	return noun("extensions", "Extensions of a postgres slice's database", get, set)
}
