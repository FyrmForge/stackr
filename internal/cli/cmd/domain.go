package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newDomainCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domain",
		Short: "Manage domain resources (hosts tiles generate hostnames under)",
	}
	cmd.AddCommand(domainLsCmd(rt), domainAddCmd(rt), domainRmResourceCmd(rt))
	return cmd
}

func domainLsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List domain resources",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, args []string) error { return runDomainLs(cmd, rt) },
	}
}

func runDomainLs(cmd *cobra.Command, rt *Runtime) error {
	client, err := rt.Client()
	if err != nil {
		return err
	}
	rs, err := client.DomainResources(cmd.Context())
	if err != nil {
		return err
	}
	if rt.JSON {
		return rt.EmitJSON(rs)
	}
	cells := make([][]string, len(rs))
	for i, r := range rs {
		cells[i] = []string{r.ID, r.Level, r.OwnerID, r.Host}
		// trailing column only when set, matching the old CLI's piped shape
		if r.IncludeEnvOnDefault {
			cells[i] = append(cells[i], "env-on-default")
		}
	}
	rt.Table([]string{"ID", "LEVEL", "OWNER", "HOST", ""}, cells)
	return nil
}

func domainAddCmd(rt *Runtime) *cobra.Command {
	var level, owner, stack string
	var includeEnv bool
	cmd := &cobra.Command{
		Use:   "add <host>",
		Short: "Add a domain resource",
		Long: `Add a resource at a level. Stack level defaults its owner to the linked
stack; org level needs --owner <org-id>; instance level is admin-only.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if level == "" {
				level = "instance"
			}
			if level == "stack" && owner == "" {
				owner, err = rt.LinkedStack(stack)
				if err != nil {
					return err
				}
			}
			r, err := client.AddDomainResource(cmd.Context(), args[0], level, owner, includeEnv)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(r)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Added %s at %s level (%s)\n", r.Host, r.Level, r.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&level, "level", "instance", "level: instance, org or stack")
	f.StringVar(&owner, "owner", "", "owning org/stack id")
	f.BoolVar(&includeEnv, "include-env-on-default", false, "include the env in default hostnames")
	f.StringVar(&stack, "stack", "", "stack id (stack level owner fallback)")
	return cmd
}

func domainRmResourceCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm <host-or-id>",
		Aliases: []string{"remove"},
		Short:   "Remove a domain resource",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			rs, err := client.DomainResources(cmd.Context())
			if err != nil {
				return err
			}
			key := args[0]
			id := ""
			for _, r := range rs {
				if r.ID == key || r.Host == key {
					id = r.ID
					break
				}
			}
			if id == "" {
				return fmt.Errorf("no domain resource matching %s", key)
			}
			if err := rt.Confirm(fmt.Sprintf("Remove domain resource %s?", key)); err != nil {
				return err
			}
			if err := client.DeleteDomainResource(cmd.Context(), id); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{id, true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed.")
			return nil
		},
	}
	return cmd
}

// legacyDomainsCmd keeps `stackr domains resources list|add|remove` working.
func legacyDomainsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "domains",
		Hidden: true,
		Short:  "Legacy alias for `stackr domain`",
	}
	resources := &cobra.Command{
		Use:   "resources",
		Short: "Legacy alias for `stackr domain`",
		Args:  cobra.NoArgs,
		// bare `domains resources` listed
		RunE: func(cmd *cobra.Command, args []string) error { return runDomainLs(cmd, rt) },
	}
	rm := domainRmResourceCmd(rt)
	resources.AddCommand(domainLsCmd(rt), domainAddCmd(rt), rm)
	cmd.AddCommand(resources)
	return cmd
}
