package main

import (
	"net/url"

	"github.com/spf13/cobra"
)

// routes are hosts that go to an address outside stackr (admin): the noun
// under `stackr admin`.
func (a *app) routes() *cobra.Command {
	var to, mode string
	var insecure bool
	ls := leaf("ls", "admin.route-list", "List routes", exact(0), func(*cobra.Command, []string) error {
		v, err := a.call(GET, "/admin/routes", nil)
		if err != nil {
			return err
		}
		if a.json {
			return a.show(v)
		}
		rs, _ := v.([]any)
		rows := make([][]string, 0, len(rs))
		for _, it := range rs {
			r, _ := it.(map[string]any)
			rows = append(rows, []string{
				cell(r["host"]),
				cell(r["mode"]),
				cell(r["target"]),
				cell(r["insecure"]),
			})
		}
		a.table([]string{"host", "mode", "target", "insecure"}, rows)
		return nil
	})
	add := leaf("add <host>", "admin.route-create", "Add a route", exact(1), func(_ *cobra.Command, args []string) error {
		v, err := a.call(POST, "/admin/routes", map[string]any{
			"host":     args[0],
			"mode":     mode,
			"target":   to,
			"insecure": insecure,
		})
		if err != nil {
			return err
		}
		return a.show(v, "host", "mode", "target", "insecure", "id")
	})
	add.Flags().StringVar(&to, "to", "", "where it goes: host or host:port (443 by default, 80 for http)")
	add.Flags().StringVar(&mode, "mode", "", "passthrough (raw TLS, by name), http or https (stackr terminates TLS)")
	add.Flags().BoolVar(&insecure, "insecure", false, "https only: do not verify the target's certificate")
	_ = add.MarkFlagRequired("to")
	_ = add.MarkFlagRequired("mode")
	rm := leaf(
		"rm <host>",
		"admin.route-list,admin.route-delete",
		"Remove a route",
		exact(1),
		func(_ *cobra.Command, args []string) error {
			cur, err := a.find("/admin/routes", "route", args[0], "id", "host")
			if err != nil {
				return err
			}
			if err := a.confirm("Remove the route for " + cell(cur["host"]) + "? The host stops being served."); err != nil {
				return err
			}
			_, err = a.call(DELETE, "/admin/routes/"+url.PathEscape(cell(cur["id"])), nil)
			return err
		},
	)
	return noun("route", "Routes: hosts that go to an address outside stackr (admin)", ls, add, rm)
}
