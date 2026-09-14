package cmd

// Releases and promotion, and the environment lifecycle verbs that went with
// them. Promoting was a button on the canvas and nothing else, which meant a
// pipeline could build a commit and then had to ask a person to move it up.

import (
	"fmt"
	"strings"

	huh "charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

func newReleasesCmd(rt *Runtime) *cobra.Command {
	var stackFlag string
	cmd := &cobra.Command{
		Use:     "releases",
		Aliases: []string{"release"},
		Short:   "List the commits this stack can promote",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := rt.LinkedStack(stackFlag)
			if err != nil {
				return err
			}
			rs, err := client.Releases(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(rs)
			}
			rows := make([][]string, len(rs))
			for i, r := range rs {
				rows[i] = []string{shortSHA(r.Commit), builtLabel(r),
					orNone(strings.Join(r.RunningOn, ",")), orNone(r.PendingPlan)}
			}
			rt.Table([]string{"COMMIT", "STATE", "RUNNING ON", "PENDING PLAN"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	return cmd
}

func builtLabel(r cli.Release) string {
	switch {
	case r.Building:
		return "building"
	case r.Built:
		return "built"
	}
	return "not built"
}

func newPromoteCmd(rt *Runtime) *cobra.Command {
	var stackFlag, envFlag, planFlag, fromFlag string
	var force bool
	cmd := &cobra.Command{
		Use:   "promote [ref]",
		Short: "Move a built commit onto a higher environment",
		Long: `Move a built commit onto a higher environment.

ref is a full or short sha, "head" (the newest release), "latest" (the newest
built one), or --from <env> for whatever that environment runs. With no ref on
a terminal you get a picker; with --json or -y a ref is required, because there
is nobody to ask.

--plan applies a waiting config plan in the same job: the images must not move
until the config they need has landed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if envFlag == "" {
				return usagef("--env <slug> is required")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := rt.LinkedStack(stackFlag)
			if err != nil {
				return err
			}
			rs, err := client.Releases(cmd.Context(), id)
			if err != nil {
				return err
			}
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			commit, err := pickRelease(rt, rs, ref, fromFlag)
			if err != nil {
				return err
			}
			res, err := client.Promote(cmd.Context(), id, envFlag, commit, planFlag, force)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(res)
			}
			if res.Plan != "" {
				_, _ = fmt.Fprintf(rt.Stdout, "Applying plan %s, then promoting %s to %s\n",
					res.Plan, shortSHA(res.Commit), res.Env)
				return nil
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Promoted %s to %s\n", shortSHA(res.Commit), res.Env)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	f.StringVar(&envFlag, "env", "", "environment slug to promote to (required)")
	f.StringVar(&fromFlag, "from", "", "promote whatever this environment runs")
	f.StringVar(&planFlag, "plan", "", "apply this config plan first, in the same job")
	f.BoolVar(&force, "force", false, "apply the plan even when the gate would refuse it")
	return cmd
}

// pickRelease resolves the ref a promote names. Client-side on purpose: the
// server takes a commit, and "the one staging runs" is a question the list
// already answers.
func pickRelease(rt *Runtime, rs []cli.Release, ref, from string) (string, error) {
	if from != "" {
		for _, r := range rs {
			for _, e := range r.RunningOn {
				if e == from {
					return r.Commit, nil
				}
			}
		}
		return "", fmt.Errorf("no release is running on %s", from)
	}
	switch ref {
	case "":
	case "head":
		if len(rs) == 0 {
			return "", fmt.Errorf("this stack has no releases yet")
		}
		return rs[0].Commit, nil
	case "latest":
		for _, r := range rs {
			if r.Built {
				return r.Commit, nil
			}
		}
		return "", fmt.Errorf("nothing is built yet")
	default:
		var match string
		for _, r := range rs {
			if !strings.HasPrefix(r.Commit, ref) {
				continue
			}
			if match != "" && match != r.Commit {
				return "", fmt.Errorf("%q matches more than one commit", ref)
			}
			match = r.Commit
		}
		if match == "" {
			// Not in the list is not necessarily wrong: the window is the last
			// few deployments per tile. Pass it through and let the server say.
			return ref, nil
		}
		return match, nil
	}
	// No ref. There is nobody to ask in a script, and picking one for them is
	// how the wrong commit reaches production.
	if rt.JSON || rt.Yes {
		return "", usagef("a commit is required with --json or -y")
	}
	if len(rs) == 0 {
		return "", fmt.Errorf("this stack has no releases yet")
	}
	opts := make([]huh.Option[string], 0, len(rs))
	for _, r := range rs {
		label := shortSHA(r.Commit) + "  " + builtLabel(r)
		if len(r.RunningOn) > 0 {
			label += "  on " + strings.Join(r.RunningOn, ",")
		}
		opts = append(opts, huh.NewOption(label, r.Commit))
	}
	var picked string
	if err := rt.form(huh.NewSelect[string]().Title("Promote which commit?").
		Options(opts...).Value(&picked)).Run(); err != nil {
		return "", err
	}
	return picked, nil
}

// ---- environment lifecycle ----

// envTarget resolves an environment id from a slug within the linked stack, so
// the CLI speaks the name a person reads off the ladder.
func envTarget(cmd *cobra.Command, rt *Runtime, client *cli.Client, stackFlag, slug string) (string, error) {
	id, err := rt.LinkedStack(stackFlag)
	if err != nil {
		return "", err
	}
	es, err := client.Envs(cmd.Context(), id)
	if err != nil {
		return "", err
	}
	for _, e := range es {
		if e.Slug == slug || e.ID == slug {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("no environment %s in this stack", slug)
}

func envRmCmd(rt *Runtime) *cobra.Command {
	var stackFlag string
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <env>",
		Aliases: []string{"delete"},
		Short:   "Delete an environment and everything deployed in it",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := envTarget(cmd, rt, client, stackFlag, args[0])
			if err != nil {
				return err
			}
			if err := rt.Confirm("Delete " + args[0] + "? Its containers go with it; database volumes are kept."); err != nil {
				return err
			}
			// -y answered the prompt, which is the same deliberate act force is.
			if err := client.DeleteEnv(cmd.Context(), id, force || rt.Yes); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Deleted "+args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	cmd.Flags().BoolVar(&force, "force", false, "tear down running tiles as well")
	return cmd
}

func envResetCmd(rt *Runtime) *cobra.Command {
	var stackFlag string
	var force bool
	cmd := &cobra.Command{
		Use:   "reset <env>",
		Short: "Tear a config-managed environment down so the next apply rebuilds it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := envTarget(cmd, rt, client, stackFlag, args[0])
			if err != nil {
				return err
			}
			if err := rt.Confirm("Reset " + args[0] + "? Everything in it is torn down and rebuilt by the next apply."); err != nil {
				return err
			}
			if err := client.ResetEnv(cmd.Context(), id, force || rt.Yes); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Reset "+args[0]+". Apply the plan to build it again.")
			return nil
		},
	}
	cmd.Flags().StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	cmd.Flags().BoolVar(&force, "force", false, "tear down running tiles as well")
	return cmd
}

func envCopyCmd(rt *Runtime) *cobra.Command {
	var stackFlag string
	var ephemeral bool
	cmd := &cobra.Command{
		Use:   "copy <env> <new-name>",
		Short: "Create an environment from an existing one; nothing is deployed",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := envTarget(cmd, rt, client, stackFlag, args[0])
			if err != nil {
				return err
			}
			e, err := client.CopyEnv(cmd.Context(), id, args[1], ephemeral)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(envRow{e.ID, e.Name, e.Slug})
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Created %s (%s). Nothing is deployed yet.\n", e.Name, e.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	cmd.Flags().BoolVar(&ephemeral, "ephemeral", false, "a disposable clone rather than a rung on the ladder")
	return cmd
}

func envSetCmd(rt *Runtime) *cobra.Command {
	var stackFlag, color, policy string
	cmd := &cobra.Command{
		Use:   "set <env>",
		Short: "Change an environment's colour or apply policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fields := map[string]any{}
			if cmd.Flags().Changed("color") {
				fields["color"] = color
			}
			if cmd.Flags().Changed("apply-policy") {
				fields["apply_policy"] = policy
			}
			if len(fields) == 0 {
				return usagef("nothing to set; pass --color or --apply-policy")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := envTarget(cmd, rt, client, stackFlag, args[0])
			if err != nil {
				return err
			}
			e, err := client.PatchEnv(cmd.Context(), id, fields)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(envRow{e.ID, e.Name, e.Slug})
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Updated %s\n", e.Slug)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	f.StringVar(&color, "color", "", "palette name or #rrggbb; empty inherits")
	f.StringVar(&policy, "apply-policy", "", "auto | manual; empty is the per-rung default")
	return cmd
}
