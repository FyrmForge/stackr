package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var serverPlanCols = []string{"id", "status", "source", "summary", "commit", "created_at"}

// serverConfig is the server's stackr-server.yml: the binding, its plans,
// the approve that applies one, a local file applied from any machine with an
// admin key, a preview and the export. Admin only; the routes are org-less.
func (a *app) serverConfig() *cobra.Command {
	var repo, branch, path, conn string
	var auto, unbind bool
	bind := leaf("bind", "admin.config-repo-get,admin.config-repo",
		"Show the server's config repo; --repo binds one and plans it, --unbind drops it", exact(0),
		func(c *cobra.Command, _ []string) error {
			cols := []string{"connector_id", "repo", "branch", "path", "auto"}
			switch bindMode(c, unbind) {
			case showBinding:
				v, err := a.call(GET, "/admin/config-repo", nil)
				if err != nil {
					return err
				}
				return a.show(v, cols...)
			case badBinding:
				return usage("pass --repo to bind a config repo, or --unbind to drop it")
			case dropBinding:
				if err := a.confirm("Unbind the server's config repo? Pushes to stackr-server.yml stop planning."); err != nil {
					return err
				}
				repo = ""
			}
			v, err := a.call(PUT, "/admin/config-repo", map[string]any{
				"connector_id": conn,
				"repo":         repo,
				"branch":       branch,
				"path":         path,
				"auto":         auto,
			})
			if err != nil {
				return err
			}
			return a.show(v, cols...)
		})
	bind.Flags().StringVar(&repo, "repo", "", "the repo to bind, owner/name")
	bind.Flags().BoolVar(&unbind, "unbind", false, "drop the binding (asks first; -y skips)")
	bind.Flags().StringVar(&branch, "branch", "", "the branch (empty: the repo's default)")
	bind.Flags().StringVar(&path, "path", "", "the file in the repo (empty: stackr-server.yml)")
	bind.Flags().StringVar(&conn, "connector", "", "the server connector's id")
	bind.Flags().BoolVar(&auto, "auto", false, "apply a plan with no impact line and no removal row without an approve")

	var file string
	var detailed bool
	preview := leaf("preview", "admin.config-plan-preview",
		"Diff a local stackr-server.yml against the server; nothing is stored", exact(0),
		func(*cobra.Command, []string) error {
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			v, err := a.call(POST, "/admin/config/plan-preview", map[string]string{"file": string(b)})
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
	preview.Flags().StringVarP(&file, "file", "f", "stackr-server.yml", "the file to preview")
	preview.Flags().BoolVar(&detailed, "detailed-exitcode", false, "exit 0 no changes, 1 blocked or error, 2 changes")

	var out string
	var force bool
	export := leaf("export", "admin.config-export", "Write the server as a stackr-server.yml (stdout, or -o FILE)",
		exact(0), func(*cobra.Command, []string) error {
			return a.export("/admin/config/export", out, force)
		})
	export.Flags().StringVarP(&out, "output", "o", "", "write here instead of stdout")
	export.Flags().BoolVar(&force, "force", false, "overwrite an existing -o file")

	var remove []string
	approve := waits(leaf("approve <plan>", "admin.config-plan-get,admin.config-plan-approve",
		"Apply a pending server plan: prints it, then asks", exact(1),
		func(c *cobra.Command, args []string) error {
			v, err := a.call(GET, "/admin/config/plans/"+args[0], nil)
			if err != nil {
				return err
			}
			return a.approveServer(c, v, remove)
		}))
	apply := waits(leaf("apply <file>", "admin.config-plan-file,admin.config-plan-approve",
		"Plan a local stackr-server.yml (not the repo's) and apply it: prints the plan, then asks", exact(1),
		func(c *cobra.Command, args []string) error {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			v, err := a.call(POST, "/admin/config/plan-file", map[string]string{"file": string(b)})
			if err != nil {
				return err
			}
			return a.approveServer(c, v, remove)
		}))
	for _, c := range []*cobra.Command{approve, apply} {
		c.Flags().StringArrayVar(&remove, "remove", nil, "tick a removal row to apply it (repeatable; the plan lists the keys)")
	}

	return noun("server", "The server's config file, stackr-server.yml (admin)",
		bind,
		leaf("plan", "admin.config-plan", "Plan the bound file at its branch's head now", exact(0),
			func(*cobra.Command, []string) error {
				v, err := a.call(POST, "/admin/config/plan", nil)
				if err != nil {
					return err
				}
				return a.serverPlan(v)
			}),
		preview,
		leaf("plans", "admin.config-plans", "List the server's last config plans", exact(0),
			func(*cobra.Command, []string) error {
				v, err := a.call(GET, "/admin/config/plans", nil)
				if err != nil {
					return err
				}
				return a.show(v, serverPlanCols...)
			}),
		leaf("plan-show <plan>", "admin.config-plan-get", "Read a server plan and its changes", exact(1),
			func(_ *cobra.Command, args []string) error {
				v, err := a.call(GET, "/admin/config/plans/"+args[0], nil)
				if err != nil {
					return err
				}
				return a.serverPlan(v)
			}),
		approve,
		apply,
		leaf("reject <plan>", "admin.config-plan-reject", "Close a pending server plan unapplied", exact(1),
			func(_ *cobra.Command, args []string) error {
				v, err := a.call(POST, "/admin/config/plans/"+args[0]+"/reject", nil)
				if err != nil {
					return err
				}
				return a.show(v, "id", "status", "summary")
			}),
		export,
		a.serverConnectors(),
	)
}

// serverPlan prints a server plan row, who made it, then its changes.
func (a *app) serverPlan(v any) error {
	if err := a.planRow(v, "id", "status", "source", "summary", "commit", "error"); err != nil {
		return err
	}
	if m, _ := v.(map[string]any); !a.json && m["source"] == "local" {
		_, _ = fmt.Fprintln(a.errw, "note: planned from a local file, not the repo")
	}
	return nil
}

// approveServer is approve and apply: print the plan row v, then approve it
// as the org verb does, following the apply job under /admin.
func (a *app) approveServer(c *cobra.Command, v any, remove []string) error {
	if err := a.serverPlan(v); err != nil {
		return err
	}
	m, _ := v.(map[string]any)
	if st := cell(m["status"]); st != "pending" {
		_, _ = fmt.Fprintf(a.errw, "nothing to apply: the plan is %s\n", st)
		return nil
	}
	id := cell(m["id"])
	body, err := a.approveBody(m, remove, "Apply server plan "+id+"?")
	if err != nil {
		return err
	}
	return a.job(c, "/admin", POST, "/admin/config/plans/"+id+"/approve", body, "server apply")
}

// approveBody checks a pending plan row before its approve goes out: refuse
// a blocked plan, check each --remove names a removal row, ask the question,
// and ask once more for a plan with impact lines. The body is nil when the
// approver has nothing to add, so a plain plan sends a plain approve.
func (a *app) approveBody(row map[string]any, remove []string, question string) (any, error) {
	pl, err := planOf(row)
	if err != nil {
		return nil, err
	}
	if bl, _ := pl["blockers"].([]any); len(bl) > 0 {
		return nil, fmt.Errorf("the plan is blocked; see the blockers above")
	}
	var keys []string
	risky := false
	changes, _ := pl["changes"].([]any)
	for _, c := range changes {
		ch, _ := c.(map[string]any)
		if k, _ := ch["key"].(string); k != "" && ch["optional"] == true {
			keys = append(keys, k)
		}
		if s, _ := ch["impact"].(string); s != "" {
			risky = true
		}
	}
	for _, k := range remove {
		if !slices.Contains(keys, k) {
			return nil, usage("--remove %s is no removal row of this plan (rows: %s)", k, cmpJoin(keys))
		}
	}
	if err := a.confirm(question); err != nil {
		return nil, err
	}
	if risky {
		if err := a.confirm("This plan has impact lines (see above). Are you sure?"); err != nil {
			return nil, err
		}
	}
	if !risky && len(remove) == 0 {
		return nil, nil
	}
	body := map[string]any{}
	if risky {
		body["confirm"] = true
	}
	if len(remove) > 0 {
		body["ticked"] = remove
	}
	return body, nil
}

func cmpJoin(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

// review prints what the changes table does not: each impact line, and each
// removal row with the flag that ticks it. Removal rows apply only when
// ticked.
func (a *app) review(pl map[string]any) {
	changes, _ := pl["changes"].([]any)
	for _, c := range changes {
		ch, _ := c.(map[string]any)
		if s, _ := ch["impact"].(string); s != "" {
			_, _ = fmt.Fprintf(a.errw, "impact: %s\n", s)
		}
		if k, _ := ch["key"].(string); k != "" && ch["optional"] == true {
			_, _ = fmt.Fprintf(a.errw, "remove?: %s (stays unless you pass --remove %s)\n", k, k)
		}
	}
}

// export writes a GET path's body to stdout or to -o.
func (a *app) export(path, out string, force bool) error {
	if out != "" && !force {
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite it", out)
		}
	}
	res, err := a.request(GET, path, nil)
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
}
