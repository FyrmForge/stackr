package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var orgPlanCols = []string{"id", "status", "summary", "commit", "created_at"}

// orgConfig is the org's stackr-org.yml: the binding, its plans, the
// approve that applies one, a local preview and the export.
func (a *app) orgConfig(org func(func(p string) error) error) []*cobra.Command {
	var repo, branch, path, conn string
	var auto bool
	bind := leaf("config-repo", "org.config-repo", "Point the org at its stackr-org.yml and plan it", exact(0),
		func(*cobra.Command, []string) error {
			return org(func(p string) error {
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
	bind.Flags().StringVar(&repo, "repo", "", "the repo, owner/name (empty: unbind)")
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
			if out != "" && !force {
				if _, err := os.Stat(out); err == nil {
					return fmt.Errorf("%s already exists; pass --force to overwrite it", out)
				}
			}
			return org(func(p string) error {
				res, err := a.request(GET, p+"/config/export", nil)
				if err != nil {
					return err
				}
				defer func() { _ = res.Body.Close() }()
				b, err := io.ReadAll(res.Body)
				if err != nil {
					return err
				}
				if out == "" {
					_, err = a.out.Write(b)
					return err
				}
				if err := os.WriteFile(out, b, 0o644); err != nil {
					return err
				}
				a.say("Wrote %s", out)
				return nil
			})
		})
	export.Flags().StringVarP(&out, "output", "o", "", "write here instead of stdout")
	export.Flags().BoolVar(&force, "force", false, "overwrite an existing -o file")

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
		waits(leaf("approve <plan>", "org.plan-approve", "Apply a pending config plan", exact(1),
			func(c *cobra.Command, args []string) error {
				return org(func(p string) error {
					return a.orgJob(c, POST, p+"/config/plans/"+args[0]+"/approve", nil, "org apply")
				})
			})),
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

// orgPlan prints a plan row, then its changes when it has any.
func (a *app) orgPlan(v any) error {
	if a.json {
		return a.show(v)
	}
	m, _ := v.(map[string]any)
	if err := a.show(m, "id", "status", "summary", "commit", "error"); err != nil {
		return err
	}
	s, _ := m["plan"].(string)
	if s == "" {
		return nil
	}
	var pl map[string]any
	if err := json.Unmarshal([]byte(s), &pl); err != nil {
		return err
	}
	a.changes(pl, "notes", "blockers")
	return nil
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
