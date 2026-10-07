package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var hostGrantCols = []string{"granted", "lines", "privileged", "approved_by", "pending"}

// hostGrant is the host access a server admin approved for a stack: host
// mounts, devices and privileged. A deploy that needs more parks until an
// admin approves it here or on the stack's page.
func (a *app) hostGrant() *cobra.Command {
	return scoped(noun("host-grant", "Host access: what a server admin approved for a stack",
		a.get("show", "hostgrant.get", "Show the stack's approved host access and what waits for approval",
			atStack, "/host-grant", hostGrantCols...),
		leaf("approve", "hostgrant.approve", "Approve the host access the stack's parked jobs ask for (server admin)", exact(0),
			a.at(atStack, func(_ *cobra.Command, p string, _ []string) error {
				v, err := a.call(GET, p+"/host-grant", nil)
				if err != nil {
					return err
				}
				g, _ := v.(map[string]any)
				ask, _ := g["pending"].([]any)
				if len(ask) == 0 {
					return fmt.Errorf("nothing waits on host access for stack %s", last(p))
				}
				var items []string
				for _, x := range ask {
					items = append(items, cell(x))
				}
				if err := a.confirm("Give stack " + last(p) + " host access to " + strings.Join(items, ", ") + "? Its parked jobs resume."); err != nil {
					return err
				}
				v, err = a.call(POST, p+"/host-grant/approve", map[string]any{"pending": items})
				if err != nil {
					return err
				}
				return a.show(v, hostGrantCols...)
			})),
		leaf("revoke", "hostgrant.revoke", "Revoke the stack's host access (server admin); running tiles stay, the next deploy that needs it parks", exact(0),
			a.at(atStack, func(_ *cobra.Command, p string, _ []string) error {
				if err := a.confirm("Revoke host access for stack " + last(p) + "? Its next deploy that needs it parks."); err != nil {
					return err
				}
				_, err := a.call(DELETE, p+"/host-grant", nil)
				return err
			})),
	), false)
}
