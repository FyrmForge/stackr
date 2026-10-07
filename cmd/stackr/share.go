package main

import (
	"net/url"

	"github.com/spf13/cobra"
)

var shareCols = []string{"slug", "kind", "source", "options", "user", "password_ref", "id"}

// shares are an org's NFS or SMB exports, mounted by tiles with a
// share:<slug>/<sub>:/path line.
func (a *app) shares() *cobra.Command {
	var kind, source, options, user, password string
	ls := leaf("ls", "share.list", "List the org's shares", exact(0), func(*cobra.Command, []string) error {
		p, err := a.orgPath()
		if err != nil {
			return err
		}
		v, err := a.call(GET, p+"/shares", nil)
		if err != nil {
			return err
		}
		return a.show(v, shareCols...)
	})
	add := leaf("add <slug>", "share.create", "Add a share", exact(1), func(_ *cobra.Command, args []string) error {
		p, err := a.orgPath()
		if err != nil {
			return err
		}
		v, err := a.call(POST, p+"/shares", map[string]any{
			"slug":         args[0],
			"kind":         kind,
			"source":       source,
			"options":      options,
			"user":         user,
			"password_ref": password,
		})
		if err != nil {
			return err
		}
		return a.show(v, shareCols...)
	})
	add.Flags().StringVar(&kind, "kind", "", "nfs or smb")
	add.Flags().StringVar(&source, "source", "", "nfs: host:/export, smb: //host/share")
	add.Flags().StringVar(&options, "options", "", "comma-separated mount options, e.g. nfsvers=4")
	add.Flags().StringVar(&user, "user", "", "smb user, plain or an ${{ org.params.<collection>.<name> }} ref")
	add.Flags().StringVar(&password, "password", "", "smb password as an ${{ org.params.<collection>.<name> }} ref, never the value")
	rm := leaf("rm <slug>", "share.delete", "Remove a share; refused while a tile mounts it", exact(1),
		func(_ *cobra.Command, args []string) error {
			p, err := a.orgPath()
			if err != nil {
				return err
			}
			if err := a.confirm("Remove share " + args[0] + "? The export itself is untouched."); err != nil {
				return err
			}
			_, err = a.call(DELETE, p+"/shares/"+url.PathEscape(args[0]), nil)
			return err
		})
	return noun("share", "Network shares: NFS or SMB exports tiles mount", ls, add, rm)
}
