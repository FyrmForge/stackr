package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var orgPlanCols = []string{"id", "status", "summary", "commit", "created_at"}

// orgConfig is the org's stackr-org.yml: the binding, its plans, the
// approve that applies one, a local preview and the export.
func (a *app) orgConfig(org func(func(p string) error) error) []*cobra.Command {
	var repo, branch, path, conn string
	var auto, unbind bool
	bind := leaf("config-repo", "org.config-repo,org.get",
		"Show the org's config repo; --repo binds one and plans it, --unbind drops it", exact(0),
		func(c *cobra.Command, _ []string) error {
			return org(func(p string) error {
				switch bindMode(c, unbind) {
				case showBinding:
					v, err := a.call(GET, p, nil)
					if err != nil {
						return err
					}
					return a.show(v, "config_repo", "config_branch", "config_path", "config_auto")
				case badBinding:
					return usage("pass --repo to bind a config repo, or --unbind to drop it")
				case dropBinding:
					if err := a.confirm("Unbind the org's config repo? Pushes to stackr-org.yml stop applying."); err != nil {
						return err
					}
					repo = ""
				}
				v, err := a.call(PUT, p+"/config-repo", map[string]any{
					"connector_id": conn,
					"repo":         repo,
					"branch":       branch,
					"path":         path,
					"auto":         auto,
				})
				if err != nil {
					return err
				}
				return a.show(v, "config_repo", "config_branch", "config_path", "config_auto")
			})
		})
	bind.Flags().StringVar(&repo, "repo", "", "the repo to bind, owner/name")
	bind.Flags().BoolVar(&unbind, "unbind", false, "drop the binding (asks first; -y skips)")
	bind.Flags().StringVar(&branch, "branch", "", "the branch (empty: the repo's default)")
	bind.Flags().StringVar(&path, "path", "", "the file in the repo (empty: stackr-org.yml)")
	bind.Flags().StringVar(&conn, "connector", "", "the connector id (stackr org connectors ls)")
	bind.Flags().BoolVar(&auto, "auto", false, "apply a plan with no blocker without an approve")

	var file string
	var detailed bool
	preview := leaf("preview", "org.plan-preview", "Diff a local stackr-org.yml against the org; nothing is stored",
		exact(0), func(*cobra.Command, []string) error {
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			return org(func(p string) error {
				v, err := a.call(POST, p+"/config/plan-preview", map[string]string{"file": string(b)})
				if err != nil {
					return err
				}
				pl, _ := v.(map[string]any)
				if a.json {
					err = a.show(v)
				} else {
					a.changes(pl, "notes", "blockers")
					a.review(pl)
					ch, _ := pl["changes"].([]any)
					if bl, _ := pl["blockers"].([]any); len(ch) == 0 && len(bl) == 0 {
						_, _ = fmt.Fprintln(a.out, "no changes")
					}
				}
				if err != nil || !detailed {
					return err
				}
				return planExit(pl)
			})
		})
	preview.Flags().StringVarP(&file, "file", "f", "stackr-org.yml", "the file to preview")
	preview.Flags().BoolVar(&detailed, "detailed-exitcode", false, "exit 0 no changes, 1 blocked or error, 2 changes")

	var out string
	var force bool
	export := leaf("export", "org.export", "Write the org as a stackr-org.yml (stdout, or -o FILE)", exact(0),
		func(*cobra.Command, []string) error {
			return org(func(p string) error { return a.export(p+"/config/export", out, force) })
		})
	export.Flags().StringVarP(&out, "output", "o", "", "write here instead of stdout")
	export.Flags().BoolVar(&force, "force", false, "overwrite an existing -o file")

	var remove []string
	approve := waits(leaf("approve <plan>", "org.plan-get,org.plan-approve", "Apply a pending config plan: prints it, then asks",
		exact(1), func(c *cobra.Command, args []string) error {
			return org(func(p string) error {
				pp := p + "/config/plans/" + args[0]
				v, err := a.call(GET, pp, nil)
				if err != nil {
					return err
				}
				if err := a.orgPlan(v); err != nil {
					return err
				}
				m, _ := v.(map[string]any)
				body, err := a.approveBody(m, remove, "Apply config plan "+args[0]+"?")
				if err != nil {
					return err
				}
				return a.orgJob(c, POST, pp+"/approve", body, "org apply")
			})
		}))
	approve.Flags().StringArrayVar(&remove, "remove", nil, "tick a removal row to apply it (repeatable; the plan lists the keys)")

	return []*cobra.Command{
		bind,
		leaf("plan", "org.plan", "Plan the bound file at its branch's head now", exact(0),
			func(*cobra.Command, []string) error {
				return org(func(p string) error {
					v, err := a.call(POST, p+"/config/plan", nil)
					if err != nil {
						return err
					}
					return a.orgPlan(v)
				})
			}),
		preview,
		leaf("plans", "org.plans", "List the org's last config plans", exact(0),
			func(*cobra.Command, []string) error {
				return org(func(p string) error {
					v, err := a.call(GET, p+"/config/plans", nil)
					if err != nil {
						return err
					}
					return a.show(v, orgPlanCols...)
				})
			}),
		leaf("plan-show <plan>", "org.plan-get", "Read a config plan and its changes", exact(1),
			func(_ *cobra.Command, args []string) error {
				return org(func(p string) error {
					v, err := a.call(GET, p+"/config/plans/"+args[0], nil)
					if err != nil {
						return err
					}
					return a.orgPlan(v)
				})
			}),
		approve,
		leaf("reject <plan>", "org.plan-reject", "Close a pending config plan unapplied", exact(1),
			func(_ *cobra.Command, args []string) error {
				return org(func(p string) error {
					v, err := a.call(POST, p+"/config/plans/"+args[0]+"/reject", nil)
					if err != nil {
						return err
					}
					return a.show(v, "id", "status", "summary")
				})
			}),
		export,
	}
}

// orgPlan prints an org plan row, then its changes when it has any.
func (a *app) orgPlan(v any) error {
	return a.planRow(v, "id", "status", "summary", "commit", "error")
}

// planRow prints a plan row's cols, then its changes, impact lines and
// removal rows when it has any.
func (a *app) planRow(v any, cols ...string) error {
	if a.json {
		return a.show(v)
	}
	m, _ := v.(map[string]any)
	if err := a.show(m, cols...); err != nil {
		return err
	}
	pl, err := planOf(m)
	if err != nil || pl == nil {
		return err
	}
	a.changes(pl, "notes", "blockers")
	a.review(pl)
	return nil
}

// planOf is a plan row's plan, stored as a JSON string (nil when none).
func planOf(m map[string]any) (map[string]any, error) {
	s, _ := m["plan"].(string)
	if s == "" {
		return nil, nil
	}
	var pl map[string]any
	return pl, json.Unmarshal([]byte(s), &pl)
}

// planExit is preview's --detailed-exitcode, as v0's: 0 nothing to do,
// 1 blocked, 2 changes.
// ponytail: no 3 (v0's destructive); no org change deletes anything yet.
func planExit(pl map[string]any) error {
	blockers, _ := pl["blockers"].([]any)
	changes, _ := pl["changes"].([]any)
	switch {
	case len(blockers) > 0:
		return exitErr(1)
	case len(changes) > 0:
		return exitErr(2)
	}
	return nil
}

// bindMode reads a config-repo command's flags. Bare, it only shows: it once
// unbound, the empty --repo default meaning "none".
type binding int

const (
	showBinding binding = iota
	setBinding
	dropBinding
	badBinding // other flags with neither --repo nor --unbind
)

func bindMode(c *cobra.Command, unbind bool) binding {
	other := false // the binding's own settings; --stack, -y and the rest don't count
	for _, f := range []string{"branch", "path", "connector", "auto"} {
		other = other || c.Flags().Changed(f)
	}
	repo, _ := c.Flags().GetString("repo")
	switch {
	case unbind && c.Flags().Changed("repo"):
		return badBinding
	case unbind:
		return dropBinding
	case c.Flags().Changed("repo") && repo == "": // --repo '' unbound unasked
		return badBinding
	case c.Flags().Changed("repo"):
		return setBinding
	case other:
		return badBinding
	}
	return showBinding
}
