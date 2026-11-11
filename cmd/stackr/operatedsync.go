package main

import "github.com/spf13/cobra"

// operatedSync is `stackr operated sync`: whether a postgres instance's primary waits for
// a standby before it confirms a commit, mirroring the API. Per-standby
// modes are `stackr operated standby sync`.
func (a *app) operatedSync() *cobra.Command {
	var limit int
	var required, preferred bool
	show := leaf("status <tile>", "operated.sync", "Show whether writes are protected by a standby now", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/sync", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if a.json || m == nil {
				return a.show(v)
			}
			a.say("%s", cell(m["status"]))
			return nil
		}))
	put := func(prefer bool) func(*cobra.Command, string, []string) error {
		return func(c *cobra.Command, p string, _ []string) error {
			body := map[string]any{"sync": prefer}
			if c.Flags().Changed("limit-ms") {
				body["sync_limit_ms"] = limit
			}
			if required && preferred {
				return usage("pass --required or --preferred, not both")
			}
			if required || preferred {
				body["sync_required"] = required
			}
			v, err := a.call(PUT, p+"/sync", body)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if a.json || m == nil {
				return a.show(v)
			}
			a.say("%s", cell(m["status"]))
			return nil
		}
	}
	on := leaf("on <tile>", "operated.sync-set", "Make the primary wait for a standby before it confirms a commit", exact(1), a.at(atTile, put(true)))
	on.Flags().IntVar(&limit, "limit-ms", 0, "the round trip in ms under which an auto standby is waited for (default 10)")
	on.Flags().BoolVar(&required, "required", false, "writes wait even when no standby answers (they freeze until one does)")
	on.Flags().BoolVar(&preferred, "preferred", false, "fall back to asynchronous when no standby answers (the default)")
	off := leaf("off <tile>", "operated.sync-set", "Stop waiting for standbys", exact(1), a.at(atTile, put(false)))
	return noun("sync", "(postgres) Synchronous replication of an instance", show, on, off)
}
