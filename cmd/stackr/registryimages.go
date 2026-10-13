package main

import (
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// The registry's images: what the org holds, its versions, and delete.

// registryImages are the verbs of `stackr registry` that read and delete
// images; login is beside them in registry().
func (a *app) registryImages() []*cobra.Command {
	ls := leaf("ls [repo]", "registry.list,registry.versions", "List the org's repositories, or one repo's versions", upTo(1),
		func(_ *cobra.Command, args []string) error {
			p, err := a.orgPath()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				v, err := a.call(GET, p+"/registry", nil)
				if err != nil {
					return err
				}
				return a.show(v, "name", "tags", "latest", "size")
			}
			v, err := a.call(GET, p+"/registry/versions?repo="+url.QueryEscape(args[0]), nil)
			if err != nil {
				return err
			}
			return a.show(v, "tag", "platforms", "size", "created_at", "source", "upstream_ref", "used_by")
		})
	ls.Example = "  stackr registry ls\n  stackr registry ls acme/shop/api"

	var force bool
	rm := leaf("rm <repo>:<tag>", "registry.versions,registry.delete", "Delete a version and every other tag on its digest", exact(1),
		func(_ *cobra.Command, args []string) error {
			p, err := a.orgPath()
			if err != nil {
				return err
			}
			i := strings.LastIndex(args[0], ":")
			if i <= 0 || i == len(args[0])-1 {
				return usage("name the version as <repo>:<tag>, e.g. acme/shop/api:v1")
			}
			repo, tag := args[0][:i], args[0][i+1:]
			q := "?repo=" + url.QueryEscape(repo) + "&tag=" + url.QueryEscape(tag)
			// The versions say which envs run it, so the prompt names them.
			vs, err := a.call(GET, p+"/registry/versions?repo="+url.QueryEscape(repo), nil)
			if err != nil {
				return err
			}
			warn := "Delete " + args[0] + "? It cannot be pulled again."
			xs, _ := vs.([]any)
			for _, x := range xs {
				if m, _ := x.(map[string]any); cell(m["tag"]) == tag {
					if used := strs(m["used_by"]); len(used) > 0 {
						if !force {
							return needsForce(args[0], used)
						}
						warn = args[0] + " runs in " + strings.Join(used, ", ") + ". Delete it anyway? Those envs fail to pull it on their next deploy."
					}
				}
			}
			if err := a.confirm(warn); err != nil {
				return err
			}
			if force {
				q += "&force=true"
			}
			_, err = a.call(DELETE, p+"/registry/versions"+q, nil)
			return err
		})
	rm.Example = "  stackr registry rm acme/shop/api:v1"
	rm.Flags().BoolVar(&force, "force", false, "delete even when an env runs it")
	return []*cobra.Command{ls, rm}
}

func needsForce(ref string, envs []string) error {
	return usage("%s runs in %s; pass --force to delete it anyway", ref, strings.Join(envs, ", "))
}

// registryAdmin is `stackr admin registry`: the server-wide view and the
// cleanup.
func (a *app) registryAdmin(clean *cobra.Command) *cobra.Command {
	ls := leaf("ls", "admin.registry", "Show what each org holds and how the last cleanup went", exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/admin/registry", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			m := asMap(v)
			if err := a.show(m["orgs"], "org", "versions", "bytes"); err != nil {
				return err
			}
			a.say("Total: %s versions, %s bytes.", cell(m["versions"]), cell(m["bytes"]))
			if last := asMap(m["last_clean"]); len(last) > 0 {
				a.say("Last cleanup: %s, %s versions deleted, %s bytes freed.", cell(last["at"]), cell(last["deleted"]), cell(last["freed"]))
			} else {
				a.say("No cleanup has run yet.")
			}
			return nil
		})
	return noun("registry", "The image registry, server-wide", ls, clean)
}
