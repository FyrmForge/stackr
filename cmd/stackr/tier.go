package main

import (
	"net/url"

	"github.com/spf13/cobra"
)

var tierCols = []string{"slug", "position", "locked", "id"}

// tiers is the org's env ladder: a stack env whose slug is a tier's takes
// its lock and its org param block.
func (a *app) tiers() *cobra.Command {
	// org runs f with the org path.
	org := func(f func(p string, args []string) error) func(*cobra.Command, []string) error {
		return func(_ *cobra.Command, args []string) error {
			p, err := a.orgPath()
			if err != nil {
				return err
			}
			return f(p+"/tiers", args)
		}
	}
	lock := func(use, short string, locked bool) *cobra.Command {
		return leaf(use+" <slug>", "tier.lock", short, exact(1), org(func(p string, args []string) error {
			v, err := a.call(PUT, p+"/"+url.PathEscape(args[0])+"/lock", map[string]bool{"locked": locked})
			if err != nil {
				return err
			}
			return a.show(v, tierCols...)
		}))
	}
	return noun("tier", "The org's env tiers, bottom first",
		leaf("ls", "tier.list", "List tiers", exact(0), org(func(p string, _ []string) error {
			v, err := a.call(GET, p, nil)
			if err != nil {
				return err
			}
			return a.show(v, tierCols...)
		})),
		leaf("add <slug>", "tier.create", "Add a locked tier on top of the ladder", exact(1), org(func(p string, args []string) error {
			v, err := a.call(POST, p, map[string]string{"slug": args[0]})
			if err != nil {
				return err
			}
			return a.show(v, tierCols...)
		})),
		leaf("rename <slug> <new>", "tier.rename", "Rename a tier; stack envs keep their slugs and leave it", exact(2), org(func(p string, args []string) error {
			v, err := a.call(PUT, p+"/"+url.PathEscape(args[0]), map[string]string{"slug": args[1]})
			if err != nil {
				return err
			}
			return a.show(v, tierCols...)
		})),
		leaf("order <slug>...", "tier.order", "Set the ladder, bottom first: every tier once", atLeast(1), org(func(p string, args []string) error {
			_, err := a.call(PUT, p+"/order", map[string][]string{"slugs": args})
			return err
		})),
		leaf("rm <slug>", "tier.delete", "Delete a tier no stack env is in", exact(1), org(func(p string, args []string) error {
			if err := a.confirm("Delete tier " + args[0] + "? Its org params are deleted with it."); err != nil {
				return err
			}
			_, err := a.call(DELETE, p+"/"+url.PathEscape(args[0]), nil)
			return err
		})),
		lock("lock", "Lock a tier: other envs cannot read its org params", true),
		lock("unlock", "Unlock a tier", false),
	)
}
