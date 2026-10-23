package main

import (
	"github.com/spf13/cobra"
)

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

	var dns bool
	ls := leaf("ls", "admin.connector-list,admin.dns-list", "List the server's git connectors; --dns its DNS ones", exact(0),
		func(*cobra.Command, []string) error {
			if dns {
				v, err := a.call(GET, "/admin/dns-connectors", nil)
				if err != nil {
					return err
				}
				return a.show(v, dnsConnCols...)
			}
			v, err := a.call(GET, "/admin/connectors", nil)
			if err != nil {
				return err
			}
			return a.show(v, serverConnectorCols...)
		})
	ls.Flags().BoolVar(&dns, "dns", false, "list the DNS connectors")

	n := noun("connectors", "Git and DNS connectors the server owns (admin)",
		ls,
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
	n.AddCommand(a.dnsVerbs(true, func(*cobra.Command) (string, error) { return "/admin/dns-connectors", nil })...)
	return n
}

var serverConnectorCols = []string{"id", "name", "host", "share_all", "org_ids", "pending", "check_error"}
