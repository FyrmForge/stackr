package main

import (
	"strings"

	"github.com/spf13/cobra"
)

// dbSettings is `stackr db settings`: a postgres instance's own settings,
// mirroring the API. The instance is the tile (--tile, or the linked one).
func (a *app) dbSettings() *cobra.Command {
	list := leaf("get", "managed.settings", "Show a postgres instance's settings and where each value comes from", exact(0),
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
	set := leaf("set <name=value>...", "managed.settings-set",
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
	unset := leaf("unset <name>...", "managed.settings-set",
		"Remove your override of settings, back to the computed or postgres default", atLeast(1), nil)
	unset.RunE = func(c *cobra.Command, args []string) error {
		return a.at(atTile, put(map[string]any{"unset": args}))(c, nil)
	}
	return scoped(noun("db", "Postgres instances",
		noun("settings", "A postgres instance's settings", list, waits(set), waits(unset))), true)
}
