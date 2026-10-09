package main

import (
	"net/url"

	"github.com/spf13/cobra"
)

// stackExport writes one env of the stack as a stackr-compose.yml.
func (a *app) stackExport() *cobra.Command {
	var out string
	var force bool
	c := leaf("export", "stack.export",
		"Write an env of the stack as a stackr-compose.yml (stdout, or -o FILE); default env: the first rung", exact(0),
		a.at(atStack, func(c *cobra.Command, p string, _ []string) error {
			if env := flag(c, "env"); env != "" {
				p += "/export?env=" + url.QueryEscape(env)
			} else {
				p += "/export"
			}
			return a.export(p, out, force)
		}))
	c.Flags().StringVarP(&out, "output", "o", "", "write here instead of stdout")
	c.Flags().BoolVar(&force, "force", false, "overwrite an existing -o file")
	return c
}
