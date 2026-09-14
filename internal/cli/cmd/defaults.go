package cmd

// The defaults cascade at every level: server, organization, stack,
// environment. One command shape for all four, because the levels are one
// cascade and reading them differently per level is how two of them drift.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// defaultsCmd builds the `set` verb for one level. resolve turns the command's
// flags into the id the API wants; the server level has none.
func defaultsCmd(rt *Runtime, kind, short string, resolve func(*cobra.Command) (string, error), register func(*cobra.Command)) *cobra.Command {
	var pairs []string
	var clear []string
	cmd := &cobra.Command{
		Use:   "set",
		Short: short,
		Long: short + `

With no --set or --clear it prints what applies here and which level decided
it. ` + "`--set key=value`" + ` overrides one knob at this level; ` + "`--clear key`" + `
gives it back to the level above.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := resolve(cmd)
			if err != nil {
				return err
			}
			fields := map[string]*string{}
			for _, p := range pairs {
				k, v, ok := strings.Cut(p, "=")
				if !ok {
					return usagef("--set takes key=value, got %q", p)
				}
				val := v
				fields[strings.TrimSpace(k)] = &val
			}
			for _, k := range clear {
				fields[strings.TrimSpace(k)] = nil
			}
			out, err := client.Settings(cmd.Context(), kind, id)
			if err != nil {
				return err
			}
			if len(fields) > 0 {
				if out, err = client.SetSettings(cmd.Context(), kind, id, fields); err != nil {
					return err
				}
			}
			if rt.JSON {
				return rt.EmitJSON(out)
			}
			rows := make([][]string, len(out.Values))
			for i, v := range out.Values {
				own := "inherited"
				if v.Own != nil {
					own = *v.Own
				}
				rows[i] = []string{v.Key, orNone(v.Value), v.Source, own}
			}
			rt.Table([]string{"KNOB", "APPLIES", "FROM", "HERE"}, rows)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&pairs, "set", nil, "key=value override at this level (repeatable)")
	f.StringArrayVar(&clear, "clear", nil, "give a knob back to the level above (repeatable)")
	if register != nil {
		register(cmd)
	}
	return cmd
}

func orgDefaultsCmd(rt *Runtime) *cobra.Command {
	var org string
	return defaultsCmd(rt, "org", "Show or change this organization's defaults",
		func(*cobra.Command) (string, error) {
			if org == "" {
				return "", usagef("--org <slug> is required; `stackr org ls` lists them")
			}
			return org, nil
		},
		func(c *cobra.Command) { c.Flags().StringVar(&org, "org", "", "organization id or slug (required)") })
}

func stackDefaultsCmd(rt *Runtime) *cobra.Command {
	var stack string
	return defaultsCmd(rt, "stack", "Show or change this stack's defaults",
		func(*cobra.Command) (string, error) { return rt.LinkedStack(stack) },
		func(c *cobra.Command) {
			c.Flags().StringVar(&stack, "stack", "", "stack id or org:stack path (default: the linked stack)")
		})
}

func envDefaultsCmd(rt *Runtime) *cobra.Command {
	var stack, env string
	return defaultsCmd(rt, "env", "Show or change this environment's defaults",
		func(cmd *cobra.Command) (string, error) {
			if env == "" {
				return "", usagef("--env <slug> is required")
			}
			client, err := rt.Client()
			if err != nil {
				return "", err
			}
			return envTarget(cmd, rt, client, stack, env)
		},
		func(c *cobra.Command) {
			c.Flags().StringVar(&stack, "stack", "", "stack id or org:stack path (default: the linked stack)")
			c.Flags().StringVar(&env, "env", "", "environment slug (required)")
		})
}

// newDefaultsCmd is the server level plus a home for the other three, so
// `stackr defaults` shows the whole cascade in one help page.
func newDefaultsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "defaults",
		Short: "The defaults cascade: server, organization, stack, environment",
		Long: `The defaults cascade.

A value set at one level applies to everything below it unless a lower level
overrides it: server, then organization, then stack, then environment, then the
tile's own explicit setting.`,
	}
	server := defaultsCmd(rt, "server", "Show or change the server defaults (admin)",
		func(*cobra.Command) (string, error) { return "", nil }, nil)
	server.Use = "server"
	org, stack, env := orgDefaultsCmd(rt), stackDefaultsCmd(rt), envDefaultsCmd(rt)
	org.Use, stack.Use, env.Use = "org", "stack", "env"
	cmd.AddCommand(server, org, stack, env)
	return cmd
}

// exportCmd writes live state back out as the config file that would produce
// it. It prints to stdout by default: the file belongs in a repo, and where in
// that repo is the author's call, not this command's.
func exportCmd(rt *Runtime, use, short string, load func(*cobra.Command) ([]byte, error), register func(*cobra.Command)) *cobra.Command {
	var out string
	var force bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + `

Prints to stdout unless --output names a file. Secrets come out as
declarations, never values: the file is meant to go into git.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := load(cmd)
			if err != nil {
				return err
			}
			if out == "" {
				_, _ = rt.Stdout.Write(body)
				return nil
			}
			if !force {
				if _, err := os.Stat(out); err == nil {
					return fmt.Errorf("%s already exists; pass --force to overwrite it", out)
				}
			}
			if err := os.WriteFile(out, body, 0o644); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Wrote "+out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write to this file instead of stdout")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	if register != nil {
		register(cmd)
	}
	return cmd
}

func stackExportCmd(rt *Runtime) *cobra.Command {
	var stack string
	return exportCmd(rt, "export", "Write this stack's live state as a stackr-compose.yml",
		func(cmd *cobra.Command) ([]byte, error) {
			client, err := rt.Client()
			if err != nil {
				return nil, err
			}
			id, err := rt.LinkedStack(stack)
			if err != nil {
				return nil, err
			}
			return client.ExportStackConfig(cmd.Context(), id)
		},
		func(c *cobra.Command) {
			c.Flags().StringVar(&stack, "stack", "", "stack id or org:stack path (default: the linked stack)")
		})
}

func orgExportCmd(rt *Runtime) *cobra.Command {
	var org string
	return exportCmd(rt, "export", "Write this organization's live state as a stackr-org.yml",
		func(cmd *cobra.Command) ([]byte, error) {
			if org == "" {
				return nil, usagef("--org <slug> is required; `stackr org ls` lists them")
			}
			client, err := rt.Client()
			if err != nil {
				return nil, err
			}
			return client.ExportOrgConfig(cmd.Context(), org)
		},
		func(c *cobra.Command) { c.Flags().StringVar(&org, "org", "", "organization id or slug (required)") })
}
