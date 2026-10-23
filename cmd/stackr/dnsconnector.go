package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

var (
	dnsConnCols   = []string{"id", "name", "kind", "created_at"}
	dnsDomainCols = []string{"name", "state", "share_all", "org_ids"}
)

// dnsVerbs are the DNS connector verbs of one level: the server's
// (`stackr server connectors`, admin; base /admin/dns-connectors, ops
// admin.dns-*) or an org's (`stackr org connectors`, owner; ops dns.*).
// base resolves the API path; server says which level.
func (a *app) dnsVerbs(server bool, base func(c *cobra.Command) (string, error)) []*cobra.Command {
	op := func(name string) string {
		if server {
			return "admin.dns-" + name
		}
		return "dns." + name
	}
	domains := leaf("domains <connector>", op("domains"),
		"List the account's domains: managed, not managed, or DNS elsewhere", exact(1),
		func(c *cobra.Command, args []string) error {
			b, err := base(c)
			if err != nil {
				return err
			}
			v, err := a.call(GET, b+"/"+url.PathEscape(args[0])+"/domains", nil)
			if err != nil {
				return err
			}
			return a.show(v, dnsDomainCols...)
		})

	var orgs []string
	manage := leaf("manage <connector> <domain>", op("manage"),
		"Let stackr manage a domain's DNS records", exact(2),
		func(c *cobra.Command, args []string) error {
			b, err := base(c)
			if err != nil {
				return err
			}
			if server && len(orgs) == 0 {
				if err := a.confirm("Make " + args[1] + " available to every organization?"); err != nil {
					return err
				}
			}
			body := map[string]any{"org_ids": append([]string{}, orgs...)}
			v, err := a.call(PUT, b+"/"+url.PathEscape(args[0])+"/domains/"+url.PathEscape(args[1]), body)
			if err != nil {
				return err
			}
			return a.show(v, dnsDomainCols...)
		})
	if server {
		manage.Short += "; --org limits who may use it (default: every organization)"
		manage.Flags().StringArrayVar(&orgs, "org", nil, "available to this organization id only (repeatable)")
	}

	unmanage := leaf("unmanage <connector> <domain>", op("unmanage"),
		"Stop managing a domain; stackr deletes the records it made, other records stay", exact(2),
		func(c *cobra.Command, args []string) error {
			b, err := base(c)
			if err != nil {
				return err
			}
			_, err = a.call(DELETE, b+"/"+url.PathEscape(args[0])+"/domains/"+url.PathEscape(args[1]), nil)
			return err
		})

	var confirm string
	takeover := leaf("takeover <connector> <domain>", op("takeover"),
		"Switch a domain to its provider's DNS; records at its current DNS host stop applying", exact(2),
		func(c *cobra.Command, args []string) error {
			b, err := base(c)
			if err != nil {
				return err
			}
			if confirm == "" && a.tty && !a.json {
				_, _ = fmt.Fprintf(a.errw, "Records at %s's current DNS host stop applying. Type the domain to confirm: ", args[1])
				line, _ := a.in.ReadString('\n')
				confirm = strings.TrimSpace(line)
			}
			if confirm == "" {
				return usage("type the domain to confirm: --confirm %s", args[1])
			}
			_, err = a.call(POST, b+"/"+url.PathEscape(args[0])+"/domains/"+url.PathEscape(args[1])+"/takeover",
				map[string]string{"confirm": confirm})
			return err
		})
	takeover.Flags().StringVar(&confirm, "confirm", "", "the domain, typed out (-y does not skip this)")

	return append(a.addConnector(server, base), domains, manage, unmanage, takeover)
}

// orgFlag adds --org to every leaf under cs: the org to work in, over the
// logged-in one.
func (a *app) orgFlag(cs ...*cobra.Command) {
	for _, c := range cs {
		if c.HasSubCommands() {
			a.orgFlag(c.Commands()...)
			continue
		}
		c.Flags().String("org", "", "the org (default: the logged-in one)")
		c.PreRunE = func(c *cobra.Command, _ []string) error {
			if o := flag(c, "org"); o != "" {
				a.orgSlug = o
			}
			return nil
		}
	}
}
