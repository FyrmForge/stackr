package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// dbSettings is `stackr db settings`: a postgres instance's own settings,
// mirroring the API. The instance is the tile (--tile, or the linked one).
func (a *app) dbSettings() *cobra.Command {
	list := leaf("get", "operated.settings", "Show a postgres instance's settings and where each value comes from", exact(0),
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
	set := leaf("set <name=value>...", "operated.settings-set",
		"Set settings; a reload applies them, or the instance restarts when one needs it", atLeast(1), nil)
	set.RunE = func(c *cobra.Command, args []string) error {
		kv := map[string]string{}
		for _, s := range args {
			k, v, ok := strings.Cut(s, "=")
			if !ok || k == "" || v == "" {
				return usage("%q is not name=value", s)
			}
			kv[k] = v
		}
		return a.at(atTile, put(map[string]any{"set": kv}))(c, nil)
	}
	unset := leaf("unset <name>...", "operated.settings-set",
		"Remove your override of settings, back to the computed or postgres default", atLeast(1), nil)
	unset.RunE = func(c *cobra.Command, args []string) error {
		return a.at(atTile, put(map[string]any{"unset": args}))(c, nil)
	}
	version := leaf("version", "operated.version", "Show the postgres version an instance runs and whether an update is out", exact(0),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
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
				a.say("Update available: %s. Run `stackr db update`.", to)
			}
			if m["eol"] == true {
				a.say("Postgres %v no longer gets security fixes (ended %s).", m["major"], cell(m["eol_date"]))
			}
			if rb, _ := m["rollback"].(map[string]any); rb != nil {
				a.say("The old data of postgres %v is kept until %s. Run `stackr db rollback-upgrade` to return to it.", rb["from"], cell(rb["until"]))
			}
			return nil
		}))
	update := waits(leaf("update", "operated.update", "Move an instance to the current minor of its postgres major; it restarts", exact(0),
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
	return scoped(noun("db", "Postgres instances",
		noun("settings", "A postgres instance's settings", list, waits(set), waits(unset)), a.dbStandby(), a.dbSync(), a.dbSwitchover(), a.dbPromote(), version, update,
		a.dbUpgrade(), a.dbRollbackUpgrade()), true)
}

// dbUpgrade is `stackr db upgrade`: move an instance to another postgres
// major, up or down. The data goes into a new volume; the old one is kept for
// a week. --check only asks whether it can, and changes nothing.
func (a *app) dbUpgrade() *cobra.Command {
	var to int
	var check bool
	c := waits(leaf("upgrade", "operated.upgrade",
		"Move an instance to another postgres major (down too); the old data is kept for 7 days. --check only asks whether it can", exact(0), nil))
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

// dbRollbackUpgrade is `stackr db rollback-upgrade`: back to the major the
// instance ran before its last change, on the data it had then.
func (a *app) dbRollbackUpgrade() *cobra.Command {
	c := waits(leaf("rollback-upgrade", "operated.upgrade-rollback",
		"Return to the postgres major before the last change, with the data it had then; what was written since is lost", exact(0), nil))
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if err := a.confirm("Roll back? Everything written since the version change is lost."); err != nil {
			return err
		}
		base, err := a.orgPath()
		if err != nil {
			return err
		}
		return a.job(c, base, POST, p+"/upgrade-rollback", nil, "rollback")
	})
	return c
}
