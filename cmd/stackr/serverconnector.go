package main

import (
	"github.com/spf13/cobra"
)

// The GitHub App handshake of a server connector runs in the admin panel
// (the manifest form posts to GitHub); everything else about one is here.
func init() {
	skipped["admin.connector-begin"] = "the GitHub App handshake runs in a browser (the manifest form posts to GitHub)"
}

// serverConnectors is `stackr server connectors`: the connectors the server
// owns and who they are shared with.
func (a *app) serverConnectors() *cobra.Command {
	var orgs []string
	var all bool
	share := leaf("share <id>", "admin.connector-share",
		"Set who may use a server connector: --org ids, --all, or neither to stop sharing", exact(1),
		func(_ *cobra.Command, args []string) error {
			switch {
			case all && len(orgs) > 0:
				return usage("--all shares with every organization; drop --org")
			case all:
				if err := a.confirm("Share connector " + args[0] +
					" with every organization? Every org owner can clone every repo its app is installed on."); err != nil {
					return err
				}
			case len(orgs) == 0:
				if err := a.confirm("Stop sharing connector " + args[0] + " with any organization?"); err != nil {
					return err
				}
			}
			v, err := a.call(PUT, "/admin/connectors/"+args[0]+"/shares", map[string]any{
				"org_ids": append([]string{}, orgs...),
				"all":     all,
			})
			if err != nil {
				return err
			}
			return a.show(v, serverConnectorCols...)
		})
	share.Flags().StringArrayVar(&orgs, "org", nil, "share with this organization id (repeatable)")
	share.Flags().BoolVar(&all, "all", false, "share with every organization")

	return noun("connectors", "Git connectors the server owns, and who they are shared with (admin)",
		leaf("ls", "admin.connector-list", "List the server's connectors", exact(0),
			func(*cobra.Command, []string) error {
				v, err := a.call(GET, "/admin/connectors", nil)
				if err != nil {
					return err
				}
				return a.show(v, serverConnectorCols...)
			}),
		leaf("rename <id> <name>", "admin.connector-rename", "Rename a server connector", exact(2),
			func(_ *cobra.Command, args []string) error {
				v, err := a.call(PUT, "/admin/connectors/"+args[0]+"/name", map[string]string{"name": args[1]})
				if err != nil {
					return err
				}
				return a.show(v, serverConnectorCols...)
			}),
		share,
		leaf("rm <id>", "admin.connector-delete", "Remove a server connector", exact(1),
			func(_ *cobra.Command, args []string) error {
				if err := a.confirm("Remove connector " + args[0] + "? Nothing may be bound to it."); err != nil {
					return err
				}
				_, err := a.call(DELETE, "/admin/connectors/"+args[0], nil)
				return err
			}),
	)
}

var serverConnectorCols = []string{"id", "name", "host", "share_all", "org_ids"}
