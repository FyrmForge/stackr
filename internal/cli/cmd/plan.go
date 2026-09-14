package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/cli"
)

func newPlanCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Config-as-code plans: preview, re-plan, approve",
		Long: `Drive config-as-code without the canvas. preview is the CI shape: post the
local file, print what would change, nothing stored. now re-plans from the
bound repo, producing real (approvable) plans.`,
		// No silent alias for the bare noun, it used to mean `now`, and
		// guessing between preview and now would surprise whoever meant the
		// other one.
	}
	cmd.AddCommand(
		planPreviewCmd(rt), planNowCmd(rt), planListCmd(rt),
		planShowCmd(rt), planApproveCmd(rt), planRejectCmd(rt),
	)
	return cmd
}

// detailedExitErr maps a previewed plan to terraform-style exit codes:
// 0 empty · 1 errors · 2 changes · 3 changes and destructive (so CI can gate
// on 3 specifically). Minting a generated secret is work too, so it counts as
// non-empty. Known wart (shared with terraform): usage errors also exit 2.
func detailedExitErr(p cli.Plan) error {
	// Rows for a declared-but-unset value are not work an apply does: counting
	// them would fail a --detailed-exitcode gate on every stack that declares a
	// secret nobody has filled in yet.
	work := 0
	for _, c := range p.Changes {
		if c.Scope == "" {
			work++
		}
	}
	switch {
	case p.Error != "" || len(p.Errors) > 0:
		return WithExitCode(1, errQuietExit)
	case work == 0 && len(p.GenSecrets) == 0:
		return nil
	case p.Destructive:
		return WithExitCode(3, errQuietExit)
	default:
		return WithExitCode(2, errQuietExit)
	}
}

// errQuietExit carries an exit code without printing anything: the plan was
// already rendered, the code is the message.
var errQuietExit = quietError{}

type quietError struct{}

func (quietError) Error() string { return "" }

// readBundle loads the main config file and every file its include: names,
// relative to the main file's dir. One level only, that is all the server
// resolves.
func readBundle(path string) (string, map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	// A 1-field mini-parse, not the server's schema: the CLI deliberately does
	// not import stackrd packages. Invalid yaml still posts, the server
	// reports the real parse error with its own (better) message.
	var f struct {
		Include []string `yaml:"include"`
	}
	_ = yaml.Unmarshal(data, &f)
	files := map[string]string{}
	total := len(data)
	dir := filepath.Dir(path)
	for _, inc := range f.Include {
		// ".." alone or "../x" escapes; a file named "..foo.yml" does not.
		clean := filepath.Clean(inc)
		if filepath.IsAbs(inc) || clean == ".." || strings.HasPrefix(clean, "../") {
			return "", nil, fmt.Errorf("include %s: must be a relative path that stays inside the config file's directory", inc)
		}
		b, err := os.ReadFile(filepath.Join(dir, inc))
		if err != nil {
			return "", nil, fmt.Errorf("include %s: %w", inc, err)
		}
		files[inc] = string(b)
		total += len(b)
	}
	if total > 1<<20 {
		return "", nil, fmt.Errorf("config bundle exceeds 1 MB")
	}
	return string(data), files, nil
}

// printPlan renders a plan for humans; scope names the layer ("stack" or
// "org") when the plan isn't env-scoped.
func printPlan(w io.Writer, p cli.Plan, scope string) {
	if p.EnvSlug != "" {
		scope = "env " + p.EnvSlug
	}
	// A preview has no ID, don't print the gap where one would go.
	id := ""
	if p.ID != "" {
		id = " " + p.ID
	}
	_, _ = fmt.Fprintf(w, "Plan%s (%s, %s): %s\n", id, p.Status, scope, p.Summary)
	if p.Error != "" {
		_, _ = fmt.Fprintln(w, "  error: "+p.Error)
	}
	for _, c := range p.Changes {
		switch c.Kind {
		case "create", "create-env":
			_, _ = fmt.Fprintf(w, "  + %s\n", changeTarget(c))
		case "delete", "delete-env":
			_, _ = fmt.Fprintf(w, "  - %s\n", changeTarget(c))
		default:
			// An addition inside an update (a new uses: entry, say) has no
			// old value, and "(none) →" in front of it is noise.
			if c.Old == "" {
				_, _ = fmt.Fprintf(w, "  ~ %s %s: %s\n", changeTarget(c), c.Field, c.New)
			} else {
				_, _ = fmt.Fprintf(w, "  ~ %s %s: %s → %s\n", changeTarget(c), c.Field, c.Old, orNone(c.New))
			}
		}
		if c.Note != "" {
			_, _ = fmt.Fprintf(w, "      %s\n", c.Note)
		}
	}
	for _, warn := range p.Warnings {
		_, _ = fmt.Fprintln(w, "  warning: "+warn)
	}
	for _, e := range p.Errors {
		_, _ = fmt.Fprintln(w, "  ! "+e)
	}
	if p.Destructive {
		_, _ = fmt.Fprintln(w, "This plan deletes running things.")
	}
	if p.Status == "pending" {
		_, _ = fmt.Fprintf(w, "Approve with: stackr plan approve %s\n", p.ID)
	}
}

func changeTarget(c cli.PlanChange) string {
	if c.Tile == "" {
		return c.Env
	}
	return c.Env + "/" + c.Tile
}

func (rt *Runtime) emitPlan(p cli.Plan, scope string) error {
	if rt.JSON {
		return rt.EmitJSON(p)
	}
	printPlan(rt.Stdout, p, scope)
	return nil
}

func planPreviewCmd(rt *Runtime) *cobra.Command {
	var file, env, stack string
	var detailed bool
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Plan a local config file against live state; stores nothing",
		Long: `Post the local config file (and its include: files, one level) and print
the throwaway plan the server computed against live state. Nothing is stored.

--detailed-exitcode: 0 empty, 1 errors, 2 changes, 3 destructive changes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			main, files, err := readBundle(file)
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			stackID, err := rt.LinkedStack(stack)
			if err != nil {
				return err
			}
			p, err := client.PlanPreview(cmd.Context(), stackID, main, files, env)
			if err != nil {
				return err
			}
			if err := rt.emitPlan(p, "stack"); err != nil {
				return err
			}
			if detailed {
				return detailedExitErr(p)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "stackr-compose.yml", "config file to preview")
	cmd.Flags().StringVar(&env, "env", "", "environment slug to scope the preview")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id (default: the linked stack)")
	cmd.Flags().BoolVar(&detailed, "detailed-exitcode", false, "terraform-style exit codes")
	return cmd
}

func planNowCmd(rt *Runtime) *cobra.Command {
	var stack string
	cmd := &cobra.Command{
		Use:   "now",
		Short: "Re-plan the linked (or --stack) stack against its config repo",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			stackID, err := rt.LinkedStack(stack)
			if err != nil {
				return err
			}
			plans, err := client.PlanNow(cmd.Context(), stackID)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(plans)
			}
			for _, p := range plans {
				printPlan(rt.Stdout, p, "stack")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "stack id (default: the linked stack)")
	return cmd
}

func planListCmd(rt *Runtime) *cobra.Command {
	var stack string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Recent plans, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			stackID, err := rt.LinkedStack(stack)
			if err != nil {
				return err
			}
			plans, err := client.Plans(cmd.Context(), stackID, limit)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(plans)
			}
			cells := make([][]string, len(plans))
			for i, p := range plans {
				cells[i] = []string{p.ID, p.Status, orNone(p.EnvSlug), p.Summary}
			}
			rt.Table([]string{"ID", "STATUS", "ENV", "SUMMARY"}, cells)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum plans to return")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id (default: the linked stack)")
	return cmd
}

func planShowCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "show <plan-id>",
		Short: "Read a plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			p, err := client.Plan(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return rt.emitPlan(p, "stack")
		},
	}
}

func planApproveCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <plan-id>",
		Short: "Apply a pending plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			// Fetch first: only a destructive plan needs a confirm.
			p, err := client.Plan(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if p.Destructive {
				if err := rt.Confirm(fmt.Sprintf("Plan %s deletes running things. Apply it?", args[0])); err != nil {
					return err
				}
			}
			p, err = client.ApprovePlan(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return rt.emitPlan(p, "stack")
		},
	}
}

func planRejectCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "reject <plan-id>",
		Short: "Close a pending plan without applying it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			p, err := client.RejectPlan(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return rt.emitPlan(p, "stack")
		},
	}
}
