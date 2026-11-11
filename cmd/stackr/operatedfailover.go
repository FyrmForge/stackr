package main

import (
	"github.com/spf13/cobra"
)

// operatedSwitchover and operatedFailover are `stackr operated switchover`
// and `stackr operated failover`: the primary of a postgres instance with a standby moves to the
// standby, planned (both run) or because the primary is down.
func (a *app) operatedSwitchover() *cobra.Command {
	to := ""
	abort := false
	c := waits(leaf("switchover <tile>", "operated.switchover,operated.switchover-abort", "(postgres) Move the primary to a standby while both run: the primary shuts down cleanly and the standby is promoted", exact(1), nil))
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

// phrase is service.PoweredOffPhrase; the CLI never imports service.
const phrase = "I powered the node off"

func (a *app) operatedFailover() *cobra.Command {
	to := ""
	gone, async := false, false
	off := ""
	c := waits(leaf("failover <tile>", "operated.promote-standby", "(postgres) Make a standby the primary when the primary is down", exact(1), nil))
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if to == "" {
			return usage("pass --to: the node of the standby to promote")
		}
		if c.Flags().Changed("powered-off") && off != phrase {
			return usage("--powered-off takes the exact phrase: " + phrase)
		}
		return a.orgJob(c, POST, p+"/promote-standby", map[string]any{"to": to, "node_gone": gone, "powered_off": off, "allow_async_target": async}, "promote")
	})
	c.Flags().StringVar(&to, "to", "", "the node of the standby to promote (name or slug)")
	c.Flags().BoolVar(&gone, "node-gone", false, "confirm the old primary's node is really gone (needed when it does not answer)")
	c.Flags().StringVar(&off, "powered-off", "", `promote without a recorded lease on the old primary; type "`+phrase+`" to say you powered its node off`)
	c.Flags().BoolVar(&async, "allow-async-target", false, "promote a standby the primary did not wait for, although one it did wait for is usable")
	return c
}
