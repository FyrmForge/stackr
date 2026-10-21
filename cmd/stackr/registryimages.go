package main

import (
	"errors"
	"fmt"
	"net/http"
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
			v, err := a.versions(p, args[0])
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
			vs, err := a.versions(p, repo)
			if err != nil {
				return err
			}
			warn := "Delete " + args[0] + "? It cannot be pulled again."
			xs, _ := vs.([]any)
			found := false
			for _, x := range xs {
				if m, _ := x.(map[string]any); cell(m["tag"]) == tag {
					found = true
					if used := strs(m["used_by"]); len(used) > 0 {
						if !force {
							return needsForce(args[0], used)
						}
						warn = args[0] + " runs in " + strings.Join(used, ", ") + ". Delete it anyway? Those envs fail to pull it on their next deploy."
					}
				}
			}
			if !found {
				return fmt.Errorf("no version %q in %s", tag, repo)
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

// versions is a repo's versions; a repo the org lacks says so by name.
func (a *app) versions(orgPath, repo string) (any, error) {
	v, err := a.call(GET, orgPath+"/registry/versions?repo="+url.QueryEscape(repo), nil)
	var e *apiErr
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil, fmt.Errorf("no repository %q", repo)
	}
	return v, err
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

// observeStatus is `admin observe`: the Observability tab's status. The knobs
// are `admin setting observe_*`.
func (a *app) observeStatus() *cobra.Command {
	return leaf("observe", "admin.observe", "Show metrics history and logs: on or off, retention, data size and free disk", exact(0),
		func(*cobra.Command, []string) error {
			v, err := a.call(GET, "/admin/observe", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			m := asMap(v)
			size := func(k string) string { return metricText(num(m[k]), "bytes") }
			on := "off"
			if m["enabled"] == true {
				on = "on"
			}
			a.say("Metrics history: %s, %s days kept, writes stop under %s free.", on, cell(m["retention_days"]), size("min_free_bytes"))
			ln := "off"
			if m["logs_enabled"] == true {
				ln = "on"
			}
			a.say("Logs: %s, %s days kept, capped at %s. Data: %s.", ln, cell(m["logs_retention_days"]), size("logs_max_bytes"), size("logs_bytes"))
			if d := num(m["logs_dropped"]); d > 0 {
				a.say("Logs dropped: %s since the panel started.", cell(m["logs_dropped"]))
			}
			a.say("Container: %s. Data: %s. Free disk: %s of %s.", cell(m["container"]), size("data_bytes"), size("disk_free"), size("disk_total"))
			if e, _ := m["error"].(string); e != "" {
				a.say("Last error: %s", e)
			}
			return nil
		})
}
