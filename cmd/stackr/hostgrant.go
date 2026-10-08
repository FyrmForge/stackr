package main

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var hostGrantCols = []string{"granted", "lines", "approved_by", "pending"}

// hostGrant is the elevated access a server admin approved for a stack, one
// "<tile> <perm>" line each: host mounts, devices, privileged, LAN access,
// server ports and host networking. A deploy that needs more parks until an
// admin approves it here or on the stack's page.
func (a *app) hostGrant() *cobra.Command {
	var only []string
	approve := leaf("approve", "hostgrant.approve", "Approve the elevated access the stack's parked jobs ask for (server admin); --only grants part of it", exact(0),
		a.at(atStack, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/host-grant", nil)
			if err != nil {
				return err
			}
			g, _ := v.(map[string]any)
			ask, _ := g["pending"].([]any)
			if len(ask) == 0 {
				return fmt.Errorf("nothing waits on elevated access for stack %s", last(p))
			}
			var items []string
			for _, x := range ask {
				items = append(items, cell(x))
			}
			grant := items
			for _, l := range only {
				if !slices.Contains(items, l) {
					return fmt.Errorf("%q is not asked for; waiting: %s", l, strings.Join(items, ", "))
				}
			}
			if len(only) > 0 {
				grant = only
			}
			if err := a.confirm("Give stack " + last(p) + " elevated access to " + strings.Join(grant, ", ") + "? Its parked jobs resume."); err != nil {
				return err
			}
			body := map[string]any{"pending": items}
			if len(only) > 0 {
				body["grant"] = only
			}
			v, err = a.call(POST, p+"/host-grant/approve", body)
			if err != nil {
				return err
			}
			return a.show(v, hostGrantCols...)
		}))
	approve.Flags().StringArrayVar(&only, "only", nil, "grant only this line (repeatable); the rest keeps waiting")

	var tile string
	revoke := leaf("revoke", "hostgrant.revoke", "Revoke the stack's elevated access, or one tile's with --tile (server admin); running tiles stay, the next deploy that needs it parks", exact(0),
		a.at(atStack, func(_ *cobra.Command, p string, _ []string) error {
			what := "stack " + last(p)
			path := p + "/host-grant"
			if tile != "" {
				what, path = "tile "+tile+" of "+what, path+"?tile="+url.QueryEscape(tile)
			}
			if err := a.confirm("Revoke elevated access for " + what + "? Its next deploy that needs it parks."); err != nil {
				return err
			}
			_, err := a.call(DELETE, path, nil)
			return err
		}))
	revoke.Flags().StringVar(&tile, "tile", "", "revoke only this tile's lines")

	return scoped(noun("host-grant", "Elevated access: what a server admin approved for a stack",
		a.get("show", "hostgrant.get", "Show the stack's approved elevated access and what waits for approval",
			atStack, "/host-grant", hostGrantCols...),
		approve,
		revoke,
	), false)
}
