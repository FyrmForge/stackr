package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// diskMoveObserve is `stkr node disk move-observe <node> [--back]`: logs and
// metrics (the stackr-observe store) onto the controller's disk, or back.
func (a *app) diskMoveObserve() *cobra.Command {
	var back bool
	c := waits(leaf("move-observe <node>", "admin.node-disk-move-observe-plan,admin.node-disk-move-observe",
		"Move logs and metrics onto the controller's disk, or back to the system disk with --back; they stop while they copy (server admin)", exact(1),
		func(c *cobra.Command, args []string) error {
			q := ""
			if back {
				q = "?back=true"
			}
			v, err := a.call(GET, nodeDiskPath(args[0], "/move-observe-plan"+q), nil)
			if err != nil {
				return err
			}
			p := asMap(v)
			if a.json {
				return a.show(v)
			}
			to := "the disk"
			if back {
				to = "the system disk"
			} else {
				a.say("cap on the disk: %s", gb(p["cap"]))
			}
			a.say("logs and metrics: %s", size(p["size_bytes"]))
			switch {
			case p["busy"] == true:
				return fmt.Errorf("data is already moving to the disk")
			case p["fits"] != true:
				return fmt.Errorf("%s has %s free and the move needs %s (the data and a tenth over)", to, gb(p["free"]), gb(p["need"]))
			}
			if err := a.confirm("Move logs and metrics to " + to + "? They stop while they copy."); err != nil {
				return err
			}
			return a.job(c, "/admin", POST, nodeDiskPath(args[0], "/move-observe"+q), nil, "move of logs and metrics")
		}))
	c.Flags().BoolVar(&back, "back", false, "move back to the system disk")
	c.Example = "  stkr node disk move-observe vps-fsn1\n  stkr node disk move-observe vps-fsn1 --back"
	return c
}
