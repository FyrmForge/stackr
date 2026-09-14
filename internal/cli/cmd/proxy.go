package cmd

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"
)

func newProxyCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Traefik escape hatch: static override and custom dynamic entries",
	}
	cmd.AddCommand(proxyShowCmd(rt), proxyOverrideCmd(rt), proxyEntriesCmd(rt), proxyEntryCmd(rt))
	return cmd
}

func proxyShowCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the current static config (YAML, paste-able)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if rt.JSON {
				// The YAML is the product here, a JSON wrapper would only be
				// re-quoted YAML.
				return usagef("proxy show is raw YAML; drop --json")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			pc, err := client.ProxyConfig(cmd.Context())
			if err != nil {
				return err
			}
			if pc.OverrideActive {
				_, _ = fmt.Fprintln(rt.Stdout, "# static override ACTIVE. Traefik.yml below is the override")
			} else {
				_, _ = fmt.Fprintln(rt.Stdout, "# generated static config (no override)")
			}
			_, _ = fmt.Fprint(rt.Stdout, pc.CurrentStatic)
			if len(pc.Entries) > 0 {
				_, _ = fmt.Fprintln(rt.Stdout)
				_, _ = fmt.Fprintln(rt.Stdout, "# custom dynamic entries:")
				names := make([]string, 0, len(pc.Entries))
				for n := range pc.Entries {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					_, _ = fmt.Fprintf(rt.Stdout, "#   custom-%s.yml\n", n)
				}
			}
			return nil
		},
	}
}

func proxyOverrideCmd(rt *Runtime) *cobra.Command {
	var file string
	var clear bool
	cmd := &cobra.Command{
		Use:   "override",
		Short: "Set or clear the static-config override (restarts Traefik)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if clear {
				if err := rt.Confirm("Clear the static override? Traefik restarts on the generated config."); err != nil {
					return err
				}
				if _, err := client.PutProxyOverride(cmd.Context(), ""); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(rt.Stdout, "Override cleared. Traefik restarts on the generated config.")
				return nil
			}
			if file == "" {
				return usagef("pass --file <path> or --clear")
			}
			body, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if err := rt.Confirm("Replace the static config with " + file + "? Traefik restarts with it."); err != nil {
				return err
			}
			if _, err := client.PutProxyOverride(cmd.Context(), string(body)); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Override saved. Traefik restarts with it.")
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "YAML file holding the whole static config")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the override")
	return cmd
}

func proxyEntriesCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "entries",
		Short: "List custom dynamic entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			pc, err := client.ProxyConfig(cmd.Context())
			if err != nil {
				return err
			}
			names := make([]string, 0, len(pc.Entries))
			for n := range pc.Entries {
				names = append(names, n)
			}
			sort.Strings(names)
			if rt.JSON {
				return rt.EmitJSON(names)
			}
			for _, n := range names {
				_, _ = fmt.Fprintln(rt.Stdout, n)
			}
			return nil
		},
	}
}

func proxyEntryCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entry",
		Short: "Manage one custom dynamic entry",
	}
	var file string
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Write a custom dynamic entry (picked up live)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return usagef("--file <path> is required")
			}
			body, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if _, err := client.PutProxyEntry(cmd.Context(), args[0], string(body)); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Entry %s written. Traefik picks it up live.\n", args[0])
			return nil
		},
	}
	set.Flags().StringVar(&file, "file", "", "YAML file for the entry (required)")
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a custom dynamic entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.Confirm(fmt.Sprintf("Remove proxy entry %s?", args[0])); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteProxyEntry(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Entry %s removed.\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(set, rm)
	return cmd
}
