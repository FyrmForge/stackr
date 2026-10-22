package main

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

var (
	dnsConnCols   = []string{"id", "name", "provider", "created_at"}
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
	var user, ip string
	var keyStdin bool
	namecheap := leaf("namecheap", op("add-namecheap"),
		"Add a Namecheap account: checks it and lists its domains", exact(0),
		func(c *cobra.Command, _ []string) error {
			b, err := base(c)
			if err != nil {
				return err
			}
			if user == "" {
				return usage("--user is required: the Namecheap username")
			}
			var key string
			if keyStdin {
				raw, err := io.ReadAll(a.in)
				if err != nil {
					return err
				}
				key = strings.TrimSpace(string(raw))
			} else if key, err = a.secret("", "STACKR_NAMECHEAP_KEY", "Namecheap API key"); err != nil {
				return err
			}
			if key == "" {
				return usage("no API key: pipe it to --key-stdin or set STACKR_NAMECHEAP_KEY")
			}
			v, err := a.call(POST, b+"/namecheap", map[string]string{"user": user, "key": key, "client_ip": ip})
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			m, _ := v.(map[string]any)
			conn, _ := m["connector"].(map[string]any)
			a.say("connector %s (%s)", cell(conn["id"]), cell(conn["name"]))
			return a.show(m["domains"], dnsDomainCols...)
		})
	namecheap.Example = "  pass show namecheap | stackr server connectors add namecheap --user jo --key-stdin --ip 203.0.113.7"
	namecheap.Flags().StringVar(&user, "user", "", "the Namecheap username")
	namecheap.Flags().BoolVar(&keyStdin, "key-stdin", false, "read the API key from stdin (else STACKR_NAMECHEAP_KEY or a prompt)")
	namecheap.Flags().StringVar(&ip, "ip", "", "this server's public IPv4, allowed in Namecheap's API settings (default: the one stackr detects)")

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
		"Stop managing a domain; its records stay as they are", exact(2),
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
		"Switch a domain to Namecheap DNS; records at its current DNS host stop applying", exact(2),
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

	return []*cobra.Command{noun("add", "Add a DNS connector", namecheap), domains, manage, unmanage, takeover}
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
