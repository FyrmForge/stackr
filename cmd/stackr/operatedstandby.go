package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// operatedStandby is `stackr operated standby`: a postgres instance's standbys,
// mirroring the API. The instance is the first argument.
func (a *app) operatedStandby() *cobra.Command {
	list := leaf("ls <tile>", "operated.standby-list", "List an instance's members: the primary and its standbys, with lag and slot size", exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/standbys", nil)
			if err != nil {
				return err
			}
			return a.show(v, "id", "role", "node", "state", "read_pool", "lag_seconds", "lag_bytes", "slot_bytes", "slot_cap_bytes", "sync_mode", "sync_in")
		}))
	node := ""
	add := leaf("add <tile>", "operated.standby-add", "Add a standby on a node; it copies the primary, then streams", exact(1),
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
	rm := leaf("rm <tile> <node|id>", "operated.standby-rm", "Remove a standby: its container, volume and slot", exact(2),
		a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
			id, err := byRef(p, args[1])
			if err != nil {
				return err
			}
			if err := a.confirm("Remove the standby " + args[1] + "? Its container, volume and slot go."); err != nil {
				return err
			}
			if _, err := a.call(DELETE, p+"/standbys/"+id, nil); err != nil {
				return err
			}
			a.say("Standby removed.")
			return nil
		}))
	rebuild := leaf("rebuild <tile> <node|id>", "operated.standby-rebuild", "Make a standby again on its node (one that needs a rebuild)", exact(2),
		a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
			id, err := byRef(p, args[1])
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
	mode := leaf("sync <tile> <node|id> <auto|always|never>", "operated.standby-sync", "Set whether the primary waits for a standby: auto (while it is close), always or never", exact(3),
		a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
			id, err := byRef(p, args[1])
			if err != nil {
				return err
			}
			if _, err := a.call(PUT, p+"/standbys/"+id+"/sync", map[string]string{"mode": args[2]}); err != nil {
				return err
			}
			a.say("Saved.")
			return nil
		}))
	fence := func(use, id string, on bool, short, done string) *cobra.Command {
		return leaf(use, id, short, exact(2),
			a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
				m, err := byRef(p, args[1])
				if err != nil {
					return err
				}
				if _, err := a.call(PUT, p+"/standbys/"+m+"/fence", map[string]bool{"fenced": on}); err != nil {
					return err
				}
				a.say("%s", done)
				return nil
			}))
	}
	fenceC := fence("fence <tile> <node|id>", "operated.standby-fence", true, "Fence a standby: stop it, take it out of the read pool and keep it down", "Standby fenced.")
	unfenceC := fence("unfence <tile> <node|id>", "operated.standby-fence", false, "Unfence a standby: it starts again and rejoins the read pool once it streams", "Standby unfenced.")
	return noun("standby", "(postgres) An instance's standbys", list, add, rm, rebuild, mode, fenceC, unfenceC)
}
