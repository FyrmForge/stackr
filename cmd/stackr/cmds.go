package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const (
	GET    = http.MethodGet
	POST   = http.MethodPost
	PUT    = http.MethodPut
	PATCH  = http.MethodPatch
	DELETE = http.MethodDelete
)

var (
	jobCols   = []string{"id", "kind", "state", "error", "created_at"}
	keyCols   = []string{"id", "name", "org_id", "created_at"}
	destCols  = []string{"id", "name", "kind", "endpoint", "bucket", "shared", "org_id"}
	orgCols   = []string{"slug", "name", "id"}
	userCols  = []string{"email", "name", "role", "active", "id"}
	imageCols = []string{"ref", "digest", "last_tag", "last_error", "id"}
	resCols   = []string{"host", "level", "include_env_on_default", "acme_email", "org_id", "stack_id", "id"}
)

// skipped are the API operations with no CLI verb, and why. The coverage
// test holds every other route to a command.
var skipped = map[string]string{
	"key.cli_code":                 "the panel's CLI authorize page calls it; stackr login waits for its code",
	"invite.get":                   "an invite link is opened in a browser",
	"invite.accept":                "an invite link is accepted in a browser",
	"connector.begin":              "the GitHub App handshake runs in a browser (the manifest form posts to GitHub)",
	"env.events":                   "the env canvas's live stream; stackr env traffic reads the same lanes",
	"domain-resource.update":       "v0's domain noun is ls, add, rm (cli-ref.md); a stack file's domains: edits its rows",
	"admin.domain-resource-update": "v0's domain noun is ls, add, rm (cli-ref.md)",
}

func (a *app) commands() []*cobra.Command {
	return append([]*cobra.Command{
		a.login(),
		a.logout(),
		a.linkCmd(),
		a.unlink(),
		a.status(),
		a.password(),
		a.knobs(),
		a.keys(),
		a.orgs(),
		a.dests(),
		a.domainResources(),
		a.jobs(),
		a.admin(),
	}, a.stackCommands()...)
}

// ---- session ----

func (a *app) login() *cobra.Command {
	var key, org, name string
	c := leaf("login <url>", "key.exchange,org.list", "Log in to a stackr server", exact(1),
		func(c *cobra.Command, args []string) error {
			server := strings.TrimRight(args[0], "/")
			if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
				return usage("the server is a URL: https://stackr.example.com")
			}
			a.cfg.Server = server
			keyOrg := ""
			if key == "" {
				code, err := a.browserLogin(server, name)
				if err != nil {
					return err
				}
				a.cfg.Key = ""
				v, err := a.call(POST, "/auth/exchange", map[string]string{"code": code})
				if err != nil {
					return err
				}
				m, _ := v.(map[string]any)
				a.cfg.Key, _ = m["token"].(string)
				k, _ := m["key"].(map[string]any)
				keyOrg, _ = k["org_id"].(string)
			} else {
				a.cfg.Key = key
			}
			v, err := a.call(GET, "/orgs", nil)
			if err != nil {
				return err
			}
			orgs, _ := v.([]any)
			a.cfg.Org = ""
			for _, o := range orgs {
				m, _ := o.(map[string]any)
				if slug := cell(m["slug"]); slug == org || m["id"] == keyOrg ||
					(org == "" && keyOrg == "" && len(orgs) == 1) {
					a.cfg.Org = slug
				}
			}
			if a.cfg.Org == "" && (org != "" || len(orgs) > 0) {
				return usage("which org? pass --org <slug> (stackr org ls after login lists them)")
			}
			if err := a.save(); err != nil {
				return err
			}
			a.say("logged in to %s, org %s", server, cell(a.cfg.Org))
			return nil
		})
	c.Flags().StringVar(&key, "with-key", "", "an API key made in the panel, instead of the browser flow")
	c.Flags().StringVar(&org, "org", "", "the org to work in (its slug)")
	host, _ := os.Hostname()
	c.Flags().StringVar(&name, "name", "cli@"+host, "the key's name, as the panel lists it")
	return c
}

func (a *app) logout() *cobra.Command {
	return leaf("logout", "", "Forget the stored key", exact(0), func(*cobra.Command, []string) error {
		a.cfg.Key = ""
		return a.save()
	})
}

func (a *app) linkCmd() *cobra.Command {
	c := leaf("link", "", "Bind this directory to a stack, env and tile", exact(0),
		func(c *cobra.Command, _ []string) error {
			dir, l := a.here()
			if wd, err := os.Getwd(); err == nil {
				dir = wd
			}
			for _, f := range []struct {
				name string
				to   *string
			}{
				{"stack", &l.Stack},
				{"env", &l.Env},
				{"tile", &l.Tile},
			} {
				if v := flag(c, f.name); v != "" {
					*f.to = v
				}
			}
			if l == (link{}) {
				return usage("nothing to link; pass --stack, --env and/or --tile")
			}
			if a.cfg.Links == nil {
				a.cfg.Links = map[string]link{}
			}
			a.cfg.Links[dir] = l
			return a.save()
		})
	// ponytail: flags only; a picker for the missing level when asked for.
	return scoped(c, true)
}

func (a *app) unlink() *cobra.Command {
	return leaf("unlink", "", "Drop this directory's link", exact(0), func(*cobra.Command, []string) error {
		dir, _ := a.here()
		delete(a.cfg.Links, dir)
		return a.save()
	})
}

func (a *app) status() *cobra.Command {
	return leaf("status", "me.get", "Show the server, the user and this directory's link", exact(0),
		func(*cobra.Command, []string) error {
			me, err := a.call(GET, "/me", nil)
			if err != nil {
				return err
			}
			m, _ := me.(map[string]any)
			_, l := a.here()
			return a.show(map[string]any{
				"server": a.cfg.Server,
				"org":    a.cfg.Org,
				"user":   m["email"],
				"stack":  l.Stack,
				"env":    l.Env,
				"tile":   l.Tile,
			}, "server", "org", "user", "stack", "env", "tile")
		})
}

func (a *app) password() *cobra.Command {
	return leaf("password", "me.password", "Change your password", exact(0), func(*cobra.Command, []string) error {
		cur, err := a.secret("", "STACKR_PASSWORD", "Current password")
		if err != nil {
			return err
		}
		next, err := a.secret("", "STACKR_NEW_PASSWORD", "New password")
		if err != nil {
			return err
		}
		_, err = a.call(PUT, "/me/password", map[string]string{"current": cur, "next": next})
		return err
	})
}

func (a *app) knobs() *cobra.Command {
	return leaf("knobs", "settings.catalogue", "List the settings knobs every defaults level takes", exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/settings", nil)
			if err != nil {
				return err
			}
			return a.show(v, "key", "type", "default", "scopes", "desc")
		})
}

func (a *app) keys() *cobra.Command {
	return noun("key", "Your API keys",
		leaf("ls", "key.list", "List your API keys", exact(0), func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/me/keys", nil)
			if err != nil {
				return err
			}
			return a.show(v, keyCols...)
		}),
		leaf("add <name>", "key.mint", "Make a key for this org; the token is shown once", exact(1),
			func(_ *cobra.Command, args []string) error {
				op, err := a.orgPath()
				if err != nil {
					return err
				}
				return a.token(a.call(POST, op+"/keys", map[string]string{"name": args[0]}))
			}),
		leaf("rm <id>", "key.revoke", "Revoke a key", exact(1), func(_ *cobra.Command, args []string) error {
			if err := a.confirm("Revoke API key " + args[0] + "? Anything using it stops working."); err != nil {
				return err
			}
			_, err := a.call(DELETE, "/me/keys/"+args[0], nil)
			return err
		}),
	)
}

// token prints a minted key: the token once, labelled.
func (a *app) token(v any, err error) error {
	if err != nil || a.json {
		if err == nil {
			err = a.show(v)
		}
		return err
	}
	m, _ := v.(map[string]any)
	a.say("token (shown once, not stored): %s", cell(m["token"]))
	return nil
}

// ---- org ----

func (a *app) orgs() *cobra.Command {
	org := func(f func(p string) error) error {
		p, err := a.orgPath()
		if err != nil {
			return err
		}
		return f(p)
	}
	get := func(path string, cols ...string) func(*cobra.Command, []string) error {
		return func(*cobra.Command, []string) error {
			return org(func(p string) error {
				v, err := a.call(GET, p+path, nil)
				if err != nil {
					return err
				}
				return a.show(v, cols...)
			})
		}
	}
	var role, email, url, user string
	invite := leaf("add", "invite.create", "Invite someone; the link is a credential", exact(0),
		func(*cobra.Command, []string) error {
			return org(func(p string) error {
				v, err := a.call(POST, p+"/invites", map[string]string{"email": email, "role": role})
				if err != nil {
					return err
				}
				return a.show(v, "id", "email", "role", "expires_at")
			})
		})
	invite.Flags().StringVar(&email, "email", "", "who it is for (empty: anyone holding the link)")
	invite.Flags().StringVar(&role, "role", "member", "owner or member")
	setRole := leaf("set <user-id>", "member.role", "Change a member's role", exact(1),
		func(_ *cobra.Command, args []string) error {
			return org(func(p string) error {
				_, err := a.call(PUT, p+"/members/"+args[0], map[string]string{"role": role})
				return err
			})
		})
	setRole.Flags().StringVar(&role, "role", "", "owner or member")
	_ = setRole.MarkFlagRequired("role")

	credAdd := leaf("add <name>", "credential.create", "Add a registry credential", exact(1),
		func(c *cobra.Command, args []string) error {
			return org(func(p string) error {
				pw, err := a.secret(flag(c, "password"), "STACKR_REGISTRY_PASSWORD", "Registry password")
				if err != nil {
					return err
				}
				v, err := a.call(POST, p+"/credentials", map[string]string{
					"name":     args[0],
					"url":      url,
					"username": user,
					"password": pw,
				})
				if err != nil {
					return err
				}
				return a.show(v, "id", "name", "url", "username")
			})
		})
	credSet := leaf(
		"set <id>",
		"credential.list,credential.update",
		"Change a registry credential; unset flags keep their value",
		exact(1),
		func(c *cobra.Command, args []string) error {
			return org(func(p string) error {
				cur, err := a.find(p+"/credentials", "credential", args[0], "id", "name")
				if err != nil {
					return err
				}
				ch, err := changed(c, map[string]string{
					"name":     "name",
					"url":      "url",
					"username": "username",
					"password": "password",
				})
				if err != nil || ch == nil {
					if err == nil {
						err = usage("nothing to set; pass --name, --url, --username or --password")
					}
					return err
				}
				body := map[string]any{
					"name":     cur["name"],
					"url":      cur["url"],
					"username": cur["username"],
					"password": "",
				}
				for k, v := range ch {
					body[k] = v
				}
				v, err := a.call(PUT, p+"/credentials/"+cell(cur["id"]), body)
				if err != nil {
					return err
				}
				return a.show(v, "id", "name", "url", "username")
			})
		},
	)
	for _, c := range []*cobra.Command{credAdd, credSet} {
		c.Flags().StringVar(&url, "url", "", "the registry host, e.g. ghcr.io")
		c.Flags().StringVar(&user, "username", "", "the registry user")
		c.Flags().String("password", "", "the password (prefer the prompt or STACKR_REGISTRY_PASSWORD)")
	}
	credSet.Flags().String("name", "", "a new name")

	create := leaf("create <name>", "org.create,org.rename,org.finish", "Make an org and switch to it", exact(1),
		func(_ *cobra.Command, args []string) error {
			v, err := a.call(POST, "/orgs", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			p := "/orgs/" + cell(m["slug"])
			if v, err = a.call(PUT, p+"/name", map[string]string{"name": args[0]}); err != nil {
				return err
			}
			m, _ = v.(map[string]any)
			p = "/orgs/" + cell(m["slug"])
			if v, err = a.call(POST, p+"/finish", nil); err != nil {
				return err
			}
			m, _ = v.(map[string]any)
			a.cfg.Org = cell(m["slug"])
			if err := a.save(); err != nil {
				return err
			}
			return a.show(v, orgCols...)
		})

	n := noun("org", "Orgs, members, invites, credentials, connectors, the org's config file",
		leaf("ls", "org.list", "List your orgs", exact(0), func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/orgs", nil)
			if err != nil {
				return err
			}
			return a.show(v, orgCols...)
		}),
		leaf("get", "org.get", "Show this org", exact(0), get("", orgCols...)),
		create,
		leaf("use <slug>", "", "Work in another org (the key must reach it)", exact(1),
			func(_ *cobra.Command, args []string) error {
				a.cfg.Org = args[0]
				return a.save()
			}),
		leaf("rename <name>", "org.rename", "Rename this org", exact(1), func(_ *cobra.Command, args []string) error {
			return org(func(p string) error {
				v, err := a.call(PUT, p+"/name", map[string]string{"name": args[0]})
				if err == nil {
					m, _ := v.(map[string]any)
					a.cfg.Org = cell(m["slug"])
					err = a.save()
				}
				return err
			})
		}),
		leaf("rm", "org.delete", "Delete this org", exact(0), func(*cobra.Command, []string) error {
			return org(func(p string) error {
				if err := a.confirm("Delete org " + a.cfg.Org + "? It must be empty of stacks first."); err != nil {
					return err
				}
				_, err := a.call(DELETE, p, nil)
				return err
			})
		}),
		noun("members", "Org members",
			leaf("ls", "member.list", "List members", exact(0), get("/members", "user_id", "role", "created_at")),
			setRole,
			leaf("rm <user-id>", "member.remove", "Remove a member", exact(1),
				func(_ *cobra.Command, args []string) error {
					return org(func(p string) error {
						if err := a.confirm("Remove member " + args[0] + " from " + a.cfg.Org + "? Their keys for it stop working."); err != nil {
							return err
						}
						_, err := a.call(DELETE, p+"/members/"+args[0], nil)
						return err
					})
				}),
		),
		noun("invites", "Pending invites",
			leaf(
				"ls",
				"invite.list",
				"List invites",
				exact(0),
				get("/invites", "email", "role", "expires_at", "used_at", "id"),
			),
			invite,
		),
		noun("creds", "Registry credentials",
			leaf(
				"ls",
				"credential.list",
				"List registry credentials",
				exact(0),
				get("/credentials", "id", "name", "url", "username"),
			),
			credAdd,
			credSet,
			leaf("rm <id>", "credential.delete", "Delete a registry credential", exact(1),
				func(_ *cobra.Command, args []string) error {
					return org(func(p string) error {
						if err := a.confirm("Delete registry credential " + args[0] + "? Pulls that need it will fail."); err != nil {
							return err
						}
						_, err := a.call(DELETE, p+"/credentials/"+args[0], nil)
						return err
					})
				}),
		),
		noun("connectors", "Git connectors (GitHub Apps)",
			leaf(
				"ls",
				"connector.list",
				"List connectors",
				exact(0),
				get("/connectors", "id", "name", "provider", "host"),
			),
			leaf("rename <id> <name>", "connector.rename", "Rename a connector", exact(2),
				func(_ *cobra.Command, args []string) error {
					return org(func(p string) error {
						_, err := a.call(PUT, p+"/connectors/"+args[0]+"/name", map[string]string{"name": args[1]})
						return err
					})
				}),
			leaf("rm <id>", "connector.delete", "Remove a connector", exact(1),
				func(_ *cobra.Command, args []string) error {
					return org(func(p string) error {
						if err := a.confirm("Remove connector " + args[0] + "? Stacks cloning through it stop getting pushes."); err != nil {
							return err
						}
						_, err := a.call(DELETE, p+"/connectors/"+args[0], nil)
						return err
					})
				}),
		),
	)
	n.AddCommand(a.orgConfig(org)...)
	return n
}

// dests is backup destinations: the org's, or with --server the shared
// ones an admin keeps.
func (a *app) dests() *cobra.Command {
	var server bool
	base := func() (string, error) {
		if server {
			return "/admin", nil
		}
		return a.orgPath()
	}
	var name, endpoint, region, bucket string
	var shared bool
	add := leaf(
		"add <name>",
		"dest.create,admin.dest-create",
		"Add an S3 destination; the bucket is dialled first",
		exact(1),
		func(c *cobra.Command, args []string) error {
			p, err := base()
			if err != nil {
				return err
			}
			ak, err := a.secret(flag(c, "access-key"), "STACKR_BACKUP_ACCESS_KEY", "Access key")
			if err != nil {
				return err
			}
			sk, err := a.secret(flag(c, "secret-key"), "STACKR_BACKUP_SECRET_KEY", "Secret key")
			if err != nil {
				return err
			}
			v, err := a.call(POST, p+"/backup-dests", map[string]any{
				"name":       args[0],
				"endpoint":   endpoint,
				"region":     region,
				"bucket":     bucket,
				"access_key": ak,
				"secret_key": sk,
				"shared":     shared,
			})
			if err != nil {
				return err
			}
			return a.show(v, destCols...)
		},
	)
	add.Flags().StringVar(&endpoint, "endpoint", "", "the S3 endpoint URL")
	add.Flags().StringVar(&region, "region", "", "the region")
	add.Flags().StringVar(&bucket, "bucket", "", "the bucket")
	add.Flags().BoolVar(&shared, "shared", false, "every org may use it (server destinations)")
	add.Flags().String("access-key", "", "prefer the prompt or STACKR_BACKUP_ACCESS_KEY")
	add.Flags().String("secret-key", "", "prefer the prompt or STACKR_BACKUP_SECRET_KEY")
	set := leaf("set <id>", "dest.list,dest.update,admin.dest-list,admin.dest-update",
		"Rename or share a destination (endpoint, bucket and keys stay: archives were written with them)", exact(1),
		func(c *cobra.Command, args []string) error {
			p, err := base()
			if err != nil {
				return err
			}
			cur, err := a.find(p+"/backup-dests", "destination", args[0], "id", "name")
			if err != nil {
				return err
			}
			ch, err := changed(c, map[string]string{"name": "name", "shared": "shared"})
			if err != nil || ch == nil {
				if err == nil {
					err = usage("nothing to set; pass --name or --shared")
				}
				return err
			}
			body := map[string]any{
				"name":       cur["name"],
				"endpoint":   cur["endpoint"],
				"region":     cur["region"],
				"bucket":     cur["bucket"],
				"shared":     cur["shared"],
				"access_key": "",
				"secret_key": "",
			}
			for k, v := range ch {
				body[k] = v
			}
			v, err := a.call(PUT, p+"/backup-dests/"+cell(cur["id"]), body)
			if err != nil {
				return err
			}
			return a.show(v, destCols...)
		})
	set.Flags().StringVar(&name, "name", "", "a new name")
	set.Flags().BoolVar(&shared, "shared", false, "every org may use it")
	c := noun("dest", "Backup destinations",
		leaf("ls", "dest.list,admin.dest-list", "List destinations", exact(0), func(*cobra.Command, []string) error {
			p, err := base()
			if err != nil {
				return err
			}
			v, err := a.call(GET, p+"/backup-dests", nil)
			if err != nil {
				return err
			}
			return a.show(v, destCols...)
		}),
		add,
		set,
		leaf(
			"rm <id>",
			"dest.delete,admin.dest-delete",
			"Remove a destination; archives in the bucket are kept",
			exact(1),
			func(_ *cobra.Command, args []string) error {
				p, err := base()
				if err != nil {
					return err
				}
				if err := a.confirm("Remove backup destination " + args[0] + "? Archives already in the bucket are kept."); err != nil {
					return err
				}
				_, err = a.call(DELETE, p+"/backup-dests/"+args[0], nil)
				return err
			},
		),
	)
	c.PersistentFlags().BoolVar(&server, "server", false, "the server-wide destinations (admin)")
	return c
}

// domainResources are the hosts stackr names tiles under. ls and rm work in
// --org, or with none on every resource on the server (admin).
func (a *app) domainResources() *cobra.Command {
	var org, level, owner, host, acme string
	var includeEnv bool
	base := func() string {
		if org == "" {
			return "/admin/domain-resources"
		}
		return "/orgs/" + url.PathEscape(org) + "/domain-resources"
	}
	ls := leaf(
		"ls",
		"domain-resource.list,admin.domain-resource-list",
		"List domain resources: the org's and the instance's, or with no --org every one (admin)",
		exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, base(), nil)
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
				owner := r["org_id"]
				if owner == nil {
					owner = r["stack_id"]
				}
				rows = append(rows, []string{
					cell(r["id"]),
					cell(r["level"]),
					cell(owner),
					cell(r["host"]),
					cell(r["include_env_on_default"]),
					cell(r["acme_email"]),
					cell(r["declared"]),
				})
			}
			header := []string{
				"id",
				"level",
				"owner",
				"host",
				"include-env",
				"acme",
				"declared",
			}
			a.table(header, rows)
			return nil
		},
	)
	add := leaf(
		"add",
		"domain-resource.create,domain-resource.create-stack,admin.domain-resource-create",
		"Add a host tiles get names under, at instance, org or stack level",
		exact(0),
		func(*cobra.Command, []string) error {
			p, err := a.resourceOwner(level, owner)
			if err != nil {
				return err
			}
			v, err := a.call(POST, p+"/domain-resources", map[string]any{
				"host":                   host,
				"include_env_on_default": includeEnv,
				"acme_email":             acme,
			})
			if err != nil {
				return err
			}
			return a.show(v, resCols...)
		},
	)
	add.Flags().StringVar(&level, "level", "", "instance (admin), org or stack")
	add.Flags().StringVar(&owner, "owner", "", "the org's slug, or the stack as org/stack (default: the logged-in org)")
	add.Flags().StringVar(&host, "host", "", "a bare hostname, e.g. example.com")
	add.Flags().BoolVar(&includeEnv, "include-env", false, "keep the env label on the default env (api.prod.shop, not api.shop)")
	add.Flags().StringVar(&acme, "acme-email", "", "the ACME account its certificates are issued on (default: the instance's)")
	rm := leaf(
		"rm <id or host>",
		"domain-resource.list,domain-resource.delete,admin.domain-resource-list,admin.domain-resource-delete",
		"Remove a domain resource; refused while tile domains carry its name",
		exact(1),
		func(_ *cobra.Command, args []string) error {
			// ponytail: an org's stack rows are not in its list; one goes by
			// the id as given, and the server 404s a wrong one.
			id := args[0]
			cur, err := a.find(base(), "domain resource", args[0], "id", "host")
			if err == nil {
				id = cell(cur["id"])
			}
			if err := a.confirm("Remove domain resource " + args[0] + "? New auto hostnames stop nesting under it."); err != nil {
				return err
			}
			_, err = a.call(DELETE, base()+"/"+url.PathEscape(id), nil)
			return err
		},
	)
	for _, c := range []*cobra.Command{ls, rm} {
		c.Flags().StringVar(&org, "org", "", "the org (its slug); without it, every resource on the server (admin)")
	}
	return noun("domain", "Domain resources: the hosts tiles get names under", ls, add, rm)
}

// resourceOwner is where a new domain resource is posted: /admin for the
// instance, the org, or the stack (org/stack; a bare stack, or no owner at
// org level, is in the logged-in org).
func (a *app) resourceOwner(level, owner string) (string, error) {
	switch level {
	case "instance":
		return "/admin", nil
	case "org":
		org := owner
		if org == "" {
			org = a.cfg.Org
		}
		if org == "" {
			return "", usage("no org; pass --owner <org slug>")
		}
		return "/orgs/" + url.PathEscape(org), nil
	case "stack":
		org, stack, ok := strings.Cut(owner, "/")
		if !ok {
			org = a.cfg.Org
			stack = owner
		}
		if org == "" || stack == "" {
			return "", usage("no stack; pass --owner <org>/<stack>")
		}
		return "/orgs/" + url.PathEscape(org) + "/stacks/" + url.PathEscape(stack), nil
	}
	return "", usage("--level is instance, org or stack, not %q", level)
}

// jobs reads jobs in the org, or with --admin anywhere.
func (a *app) jobs() *cobra.Command {
	var admin, follow bool
	base := func() (string, error) {
		if admin {
			return "/admin", nil
		}
		return a.orgPath()
	}
	log := leaf(
		"log <id>",
		"job.log,job.events,admin.job-log,admin.job-events",
		"Print a job's log; --follow waits for its end",
		exact(1),
		func(c *cobra.Command, args []string) error {
			b, err := base()
			if err != nil {
				return err
			}
			if follow {
				return a.follow(b, map[string]any{"id": args[0]}, "job")
			}
			v, err := a.call(GET, b+"/jobs/"+args[0]+"/log", nil)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			m, _ := v.(map[string]any)
			_, err = fmt.Fprint(a.out, m["log"])
			return err
		},
	)
	log.Flags().BoolVarP(&follow, "follow", "f", false, "stream until the job ends")
	var states []string
	ls := leaf("ls", "admin.jobs", "List every job (admin)", exact(0), func(*cobra.Command, []string) error {
		q := ""
		for _, s := range states {
			q += "&state=" + s
		}
		v, err := a.call(GET, "/admin/jobs?"+strings.TrimPrefix(q, "&"), nil)
		if err != nil {
			return err
		}
		return a.show(v, jobCols...)
	})
	ls.Flags().StringSliceVar(&states, "state", nil, "only these states (queued, running, waiting, done, failed, …)")
	c := noun("job", "Jobs: deploys, promotes, backups, everything queued",
		ls,
		leaf("get <id>", "job.get,admin.job-get", "Show a job", exact(1), func(_ *cobra.Command, args []string) error {
			b, err := base()
			if err != nil {
				return err
			}
			v, err := a.call(GET, b+"/jobs/"+args[0], nil)
			if err != nil {
				return err
			}
			return a.show(v, jobCols...)
		}),
		log,
		leaf("cancel <id>", "job.cancel,admin.job-cancel", "Cancel a queued or running job", exact(1),
			func(_ *cobra.Command, args []string) error {
				b, err := base()
				if err != nil {
					return err
				}
				if err := a.confirm("Cancel job " + args[0] + "? Work it already did stays done."); err != nil {
					return err
				}
				_, err = a.call(POST, b+"/jobs/"+args[0]+"/cancel", nil)
				return err
			}),
	)
	c.PersistentFlags().BoolVar(&admin, "admin", false, "a job of any org (admin)")
	return c
}

// ---- admin ----

func (a *app) admin() *cobra.Command {
	list := func(use, op, short, path string, cols ...string) *cobra.Command {
		return leaf(use, op, short, exact(0), func(*cobra.Command, []string) error {
			v, err := a.call(GET, path, nil)
			if err != nil {
				return err
			}
			return a.show(v, cols...)
		})
	}
	adminJob := func(use, op, short, path, what, q string) *cobra.Command {
		return waits(leaf(use, op, short, exact(0), func(c *cobra.Command, _ []string) error {
			if q != "" {
				if err := a.confirm(q); err != nil {
					return err
				}
			}
			var body any
			if t := flag(c, "tag"); t != "" {
				body = map[string]string{"tag": t}
			}
			return a.job(c, "/admin", POST, path, body, what)
		}))
	}
	var on bool
	setAdmin := leaf("admin <user-id>", "admin.user-admin", "Make a user a stackr admin, or not (--on=false)", exact(1),
		func(_ *cobra.Command, args []string) error {
			_, err := a.call(PUT, "/admin/users/"+args[0]+"/admin", map[string]bool{"admin": on})
			return err
		})
	setAdmin.Flags().BoolVar(&on, "on", true, "admin or not")
	upgrade := adminJob("upgrade", "admin.upgrade", "Upgrade stackr to a release", "/admin/upgrade", "upgrade",
		"Upgrade this stackr server? The panel restarts; tiles keep running.")
	upgrade.Flags().String("tag", "", "the release tag (stackr admin upgrade-check names the newest)")
	_ = upgrade.MarkFlagRequired("tag")
	var set, clear []string
	defaults := leaf(
		"defaults",
		"admin.defaults,admin.defaults-set",
		"Show or change the server's settings defaults",
		exact(0),
		func(*cobra.Command, []string) error {
			if len(set)+len(clear) == 0 {
				v, err := a.call(GET, "/admin/defaults", nil)
				if err != nil {
					return err
				}
				return a.show(v)
			}
			body := map[string]string{}
			for _, s := range set {
				k, v, err := kv(s)
				if err != nil {
					return err
				}
				body[k] = v
			}
			for _, k := range clear {
				body[k] = ""
			}
			_, err := a.call(PATCH, "/admin/defaults", body)
			return err
		},
	)
	defaults.Flags().StringArrayVar(&set, "set", nil, "knob=value (stackr knobs lists them)")
	defaults.Flags().StringArrayVar(&clear, "clear", nil, "give a knob back to the built-in default")
	return noun("admin", "Server administration (stackr admins)",
		list("orgs", "admin.orgs", "List every org", "/admin/orgs", orgCols...),
		list("users", "admin.users", "List users", "/admin/users", userCols...),
		setAdmin,
		leaf("disable <user-id>", "admin.user-disable", "Disable a user; their sessions and keys close", exact(1),
			func(_ *cobra.Command, args []string) error {
				if err := a.confirm("Disable user " + args[0] + "? Their sessions and API keys stop working; their orgs stay."); err != nil {
					return err
				}
				_, err := a.call(POST, "/admin/users/"+args[0]+"/disable", nil)
				return err
			}),
		leaf("password <user-id>", "admin.user-password", "Set a user's password", exact(1),
			func(_ *cobra.Command, args []string) error {
				pw, err := a.secret("", "STACKR_NEW_PASSWORD", "New password")
				if err != nil {
					return err
				}
				_, err = a.call(PUT, "/admin/users/"+args[0]+"/password", map[string]string{"password": pw})
				return err
			}),
		leaf("key <name>", "admin.key-mint", "Make an admin key bound to no org; the token is shown once", exact(1),
			func(_ *cobra.Command, args []string) error {
				return a.token(a.call(POST, "/admin/keys", map[string]string{"name": args[0]}))
			}),
		list("images", "admin.images", "List the images stackr knows", "/admin/images", imageCols...),
		adminJob(
			"image-check",
			"admin.image-check",
			"Check every image for a newer tag",
			"/admin/image-check",
			"image check",
			"",
		),
		leaf("version", "admin.version", "Show the server's version", exact(0), func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/admin/version", nil)
			if err != nil {
				return err
			}
			return a.show(v)
		}),
		leaf("upgrade-check", "admin.upgrade-check", "Is there a newer stackr?", exact(0),
			func(*cobra.Command, []string) error {
				v, err := a.call(GET, "/admin/upgrade", nil)
				if err != nil {
					return err
				}
				return a.show(v)
			}),
		upgrade,
		list(
			"panel-backups",
			"admin.panel-backups",
			"List backups of stackr's own database",
			"/admin/panel-backups",
			"id",
			"status",
			"created_at",
			"size_bytes",
			"error",
		),
		adminJob(
			"panel-backup",
			"admin.panel-backup",
			"Back up stackr's own database now",
			"/admin/panel-backups",
			"panel backup",
			"",
		),
		leaf("proxy-sync", "admin.proxy-sync", "Push the routing table to Caddy again", exact(0),
			func(*cobra.Command, []string) error {
				_, err := a.call(POST, "/admin/proxy/sync", nil)
				return err
			}),
		leaf(
			"setting <key> [value]",
			"admin.setting-get,admin.setting-set",
			"Read or write one server setting",
			atLeast(1),
			func(_ *cobra.Command, args []string) error {
				if len(args) > 2 {
					return usage("setting takes a key and at most one value")
				}
				if len(args) == 1 {
					v, err := a.call(GET, "/admin/settings/"+args[0], nil)
					if err != nil {
						return err
					}
					m, _ := v.(map[string]any)
					return a.show(m["value"])
				}
				_, err := a.call(PUT, "/admin/settings/"+args[0], map[string]string{"value": args[1]})
				return err
			},
		),
		defaults,
	)
}

// settingsCmd is `defaults` at a stack or env: the rung is a JSON object,
// read from getPath's "settings" and written whole to putPath; --set and
// --clear change only the knobs named.
func (a *app) settingsCmd(op, short string, lv level) *cobra.Command {
	var set, clear []string
	c := leaf("defaults", op, short, exact(0), func(c *cobra.Command, _ []string) error {
		p, err := a.path(c, lv, "")
		if err != nil {
			return err
		}
		v, err := a.call(GET, p, nil)
		if err != nil {
			return err
		}
		m, _ := v.(map[string]any)
		rung := map[string]any{}
		if s, _ := m["settings"].(string); s != "" {
			if err := json.Unmarshal([]byte(s), &rung); err != nil {
				return errors.New("the stored settings are not an object: " + s)
			}
		}
		if len(set)+len(clear) == 0 {
			return a.show(rung)
		}
		for _, s := range set {
			k, v, err := kv(s)
			if err != nil {
				return err
			}
			var x any
			if json.Unmarshal([]byte(v), &x) != nil {
				x = v // a bare word is a string
			}
			rung[k] = x
		}
		for _, k := range clear {
			delete(rung, k)
		}
		_, err = a.call(PUT, p+"/settings", rung)
		return err
	})
	c.Flags().StringArrayVar(&set, "set", nil, "knob=value (stackr knobs lists them)")
	c.Flags().StringArrayVar(&clear, "clear", nil, "give a knob back to the level above")
	return c
}
