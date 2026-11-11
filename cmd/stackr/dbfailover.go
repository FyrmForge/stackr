package main

import (
	"github.com/spf13/cobra"
)

// dbSwitchover and dbPromote are `stackr db switchover` and `stackr db
// promote`: the primary of a postgres instance with a standby moves to the
// standby, planned (both run) or because the primary is down.
func (a *app) dbSwitchover() *cobra.Command {
	to := ""
	abort := false
	c := waits(leaf("switchover", "operated.switchover,operated.switchover-abort", "Move the primary to a standby while both run: the primary shuts down cleanly and the standby is promoted", exact(0), nil))
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if abort {
			if _, err := a.call(POST, p+"/switchover/abort", nil); err != nil {
				return err
			}
			a.say("Switchover aborted. The old primary is unfenced and started again.")
			return nil
		}
		return a.orgJob(c, POST, p+"/switchover", map[string]string{"to": to}, "switchover")
	})
	c.Flags().StringVar(&to, "to", "", "the standby's node (name or slug); default is the one that replayed the most")
	c.Flags().BoolVar(&abort, "abort", false, "give up a switchover stuck before the promote: unfence and restart the old primary")
	return c
}

func (a *app) dbPromote() *cobra.Command {
	to := ""
	gone, off, async := false, false, false
	c := waits(leaf("promote", "operated.promote-standby", "Make a standby the primary when the primary is down", exact(0), nil))
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if to == "" {
			return usage("pass --to: the node of the standby to promote")
		}
		phrase := ""
		if off {
			phrase = "I powered the node off" // service.PoweredOffPhrase
		}
		return a.orgJob(c, POST, p+"/promote-standby", map[string]any{"to": to, "node_gone": gone, "powered_off": phrase, "allow_async_target": async}, "promote")
	})
	c.Flags().StringVar(&to, "to", "", "the node of the standby to promote (name or slug)")
	c.Flags().BoolVar(&gone, "node-gone", false, "confirm the old primary's node is really gone (needed when it does not answer)")
	c.Flags().BoolVar(&off, "powered-off", false, "promote without a recorded lease on the old primary: you powered its node off")
	c.Flags().BoolVar(&async, "allow-async-target", false, "promote a standby the primary did not wait for, although one it did wait for is usable")
	return c
}
