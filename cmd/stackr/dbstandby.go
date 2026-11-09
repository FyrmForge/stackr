package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// dbStandby is `stackr db standby`: a postgres instance's standbys,
// mirroring the API. The instance is the tile (--tile, or the linked one).
func (a *app) dbStandby() *cobra.Command {
	list := leaf("list", "operated.standby-list", "List an instance's members: the primary and its standbys, with lag and slot size", exact(0),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/standbys", nil)
			if err != nil {
				return err
			}
			return a.show(v, "id", "role", "node", "state", "read_pool", "lag_seconds", "lag_bytes", "slot_bytes", "slot_cap_bytes")
		}))
	node := ""
	add := leaf("add", "operated.standby-add", "Add a standby on a node; it copies the primary, then streams", exact(0),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			if node == "" {
				return usage("pass --node: the node the standby runs on")
			}
			v, err := a.call(POST, p+"/standbys", map[string]string{"node": node})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			a.say("Standby added on %s. It is listed as starting until the copy is done and it streams.", node)
			return nil
		}))
	add.Flags().StringVar(&node, "node", "", "the node the standby runs on (name or slug)")
	// byRef is the member a node name or an id names.
	byRef := func(p, ref string) (string, error) {
		v, err := a.call(GET, p+"/standbys", nil)
		if err != nil {
			return "", err
		}
		rows, _ := v.([]any)
		for _, r := range rows {
			m, _ := r.(map[string]any)
			if m["role"] == "replica" && (cell(m["id"]) == ref || cell(m["node"]) == ref) {
				return cell(m["id"]), nil
			}
		}
		return "", fmt.Errorf("no standby %q on this instance", ref)
	}
	rm := leaf("remove <node|id>", "operated.standby-rm", "Remove a standby: its container, volume and slot", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
			id, err := byRef(p, args[0])
			if err != nil {
				return err
			}
			if _, err := a.call(DELETE, p+"/standbys/"+id, nil); err != nil {
				return err
			}
			a.say("Standby removed.")
			return nil
		}))
	rebuild := leaf("rebuild <node|id>", "operated.standby-rebuild", "Make a standby again on its node (one that needs a rebuild)", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
			id, err := byRef(p, args[0])
			if err != nil {
				return err
			}
			v, err := a.call(POST, p+"/standbys/"+id+"/rebuild", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			a.say("Rebuilding the standby. It is listed as starting until the copy is done and it streams.")
			return nil
		}))
	return noun("standby", "A postgres instance's standbys", list, add, rm, rebuild)
}
