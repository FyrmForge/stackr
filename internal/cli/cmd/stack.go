package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// stackRow is the JSON output shape for stack listings, a dedicated struct,
// never the API DTO.
type stackRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type removed struct {
	ID      string `json:"id"`
	Removed bool   `json:"removed"`
}

func newStackCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Manage stacks",
	}
	cmd.AddCommand(stackLsCmd(rt), stackCreateCmd(rt), stackRenameCmd(rt), stackPREnvCmd(rt),
		stackDefaultsCmd(rt), stackExportCmd(rt), stackRmCmd(rt))
	return cmd
}

func stackLsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List stacks",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runStackLs(cmd, rt) },
	}
}

func runStackLs(cmd *cobra.Command, rt *Runtime) error {
	client, err := rt.Client()
	if err != nil {
		return err
	}
	ps, err := client.Stacks(cmd.Context())
	if err != nil {
		return err
	}
	rows := make([]stackRow, 0, len(ps))
	for _, p := range ps {
		rows = append(rows, stackRow{p.ID, p.Name})
	}
	if rt.JSON {
		return rt.EmitJSON(rows)
	}
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = []string{r.ID, r.Name}
	}
	rt.Table([]string{"ID", "NAME"}, cells)
	return nil
}

func stackCreateCmd(rt *Runtime) *cobra.Command {
	var desc, org string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a stack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackCreate(cmd, rt, args[0], desc, org)
		},
	}
	cmd.Flags().StringVar(&desc, "desc", "", "description")
	cmd.Flags().StringVar(&org, "org", "", "owning org id")
	return cmd
}

func runStackCreate(cmd *cobra.Command, rt *Runtime, name, desc, org string) error {
	client, err := rt.Client()
	if err != nil {
		return err
	}
	p, err := client.CreateStack(cmd.Context(), name, desc, org)
	if err != nil {
		return err
	}
	if rt.JSON {
		return rt.EmitJSON(stackRow{p.ID, p.Name})
	}
	_, _ = fmt.Fprintf(rt.Stdout, "Created stack %s (%s)\n", p.Name, p.ID)
	return nil
}

func stackRmCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove a stack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackRm(cmd, rt, args[0])
		},
	}
}

func runStackRm(cmd *cobra.Command, rt *Runtime, id string) error {
	if err := rt.Confirm(fmt.Sprintf("Remove stack %s and everything in it?", id)); err != nil {
		return err
	}
	client, err := rt.Client()
	if err != nil {
		return err
	}
	if err := client.DeleteStack(cmd.Context(), id); err != nil {
		return err
	}
	if rt.JSON {
		return rt.EmitJSON(removed{id, true})
	}
	_, _ = fmt.Fprintln(rt.Stdout, "Removed "+id)
	return nil
}

// ---- env ----

func newEnvCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage environments in a stack",
	}
	cmd.AddCommand(envLsCmd(rt), envCreateCmd(rt), envCopyCmd(rt), envSetCmd(rt),
		envDefaultsCmd(rt), envResetCmd(rt), envRmCmd(rt))
	return cmd
}

type envRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug,omitempty"`
}

func envLsCmd(rt *Runtime) *cobra.Command {
	var stack string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List environments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvLs(cmd, rt, stack)
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "stack id (default: the linked stack)")
	return cmd
}

func runEnvLs(cmd *cobra.Command, rt *Runtime, stackFlag string) error {
	stack, err := rt.LinkedStack(stackFlag)
	if err != nil {
		return err
	}
	client, err := rt.Client()
	if err != nil {
		return err
	}
	es, err := client.Envs(cmd.Context(), stack)
	if err != nil {
		return err
	}
	rows := make([]envRow, 0, len(es))
	for _, e := range es {
		rows = append(rows, envRow{e.ID, e.Name, e.Slug})
	}
	if rt.JSON {
		return rt.EmitJSON(rows)
	}
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = []string{r.ID, r.Name, r.Slug}
	}
	rt.Table([]string{"ID", "NAME", "SLUG"}, cells)
	return nil
}

func envCreateCmd(rt *Runtime) *cobra.Command {
	var stack string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvCreate(cmd, rt, stack, args[0])
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "stack id (default: the linked stack)")
	return cmd
}

func runEnvCreate(cmd *cobra.Command, rt *Runtime, stackFlag, name string) error {
	stack, err := rt.LinkedStack(stackFlag)
	if err != nil {
		return err
	}
	client, err := rt.Client()
	if err != nil {
		return err
	}
	e, err := client.CreateEnv(cmd.Context(), stack, name)
	if err != nil {
		return err
	}
	if rt.JSON {
		return rt.EmitJSON(envRow{e.ID, e.Name, e.Slug})
	}
	_, _ = fmt.Fprintf(rt.Stdout, "Created env %s (%s)\n", e.Name, e.ID)
	return nil
}

// ---- legacy spellings (hidden forwarding commands) ----

// legacyStacksCmd keeps `stackr stacks [create|rm|delete]` working: bare
// `stacks` lists, matching the old CLI.
func legacyStacksCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "stacks",
		Hidden: true,
		Short:  "Legacy alias for `stackr stack`",
		Args:   cobra.NoArgs,
		RunE:   func(cmd *cobra.Command, args []string) error { return runStackLs(cmd, rt) },
	}
	create := stackCreateCmd(rt)
	rm := stackRmCmd(rt)
	rm.Aliases = []string{"delete"}
	cmd.AddCommand(create, rm)
	return cmd
}

// legacyEnvsCmd keeps `stackr envs [create]` working.
func legacyEnvsCmd(rt *Runtime) *cobra.Command {
	var stack string
	cmd := &cobra.Command{
		Use:    "envs",
		Hidden: true,
		Short:  "Legacy alias for `stackr env`",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvLs(cmd, rt, stack)
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "stack id")
	cmd.AddCommand(envCreateCmd(rt))
	return cmd
}
