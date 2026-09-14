package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

func newInfraCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "infra",
		Short: "Manage shared infrastructure (instances and slices)",
	}
	cmd.AddCommand(
		infraCreateCmd(rt), infraProvisionCmd(rt), infraForkCmd(rt),
		infraLsCmd(rt), infraGetCmd(rt), infraRmCmd(rt), infraSetCmd(rt),
		infraPublicCmd(rt),
	)
	// Legacy scope-word form: `infra [org|stack|env] create <engine> <name>`.
	// The canonical spelling is `infra create --scope <s>`.
	for _, scope := range []string{"org", "stack", "env"} {
		scoped := &cobra.Command{Use: scope, Hidden: true, Short: "Legacy scope word for `infra create --scope " + scope + "`"}
		create := infraCreateCmd(rt)
		create.Flags().Set("scope", scope) //nolint:errcheck
		scoped.AddCommand(create)
		cmd.AddCommand(scoped)
	}
	return cmd
}

// resolveInfra turns a path argument into what it names, sending the linked
// environment so the path may be relative. The server does the completing.
func resolveInfra(cmd *cobra.Command, rt *Runtime, client *cli.Client, path, envID string) (cli.Target, error) {
	return client.Resolve(cmd.Context(), path, rt.linkedEnv(envID))
}

func infraCreateCmd(rt *Runtime) *cobra.Command {
	var stack, envSlug, scope string
	cmd := &cobra.Command{
		Use:     "create <s3|postgres|...> <name>",
		Aliases: []string{"prov"}, // legacy: `prov` created instances before `provision` meant slices
		Short:   "Create a shared instance; prints its colon-path address",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			stackID, err := rt.LinkedStack(stack)
			if err != nil {
				return err
			}
			db, err := client.CreateDB(cmd.Context(), stackID, args[1], args[0], envSlug, scope)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(db)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Created %s instance %s (%s)\n  %s\n", db.Engine, db.Name, db.ID, db.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "stack id (default: the linked stack)")
	cmd.Flags().StringVar(&envSlug, "env-slug", "", "environment slug")
	cmd.Flags().StringVar(&scope, "scope", "", "sharing scope: env (default), stack or org")
	return cmd
}

func infraProvisionCmd(rt *Runtime) *cobra.Command {
	var public bool
	var envID string
	cmd := &cobra.Command{
		Use:   "provision <instance-path> <name>",
		Short: "Cut a slice (logical db / bucket) out of an instance",
		Long: `Cut a slice out of a shared instance, in the linked env unless --env-id says
otherwise. Binding it to a consumer is ` + "`stackr tile attach`" + `.
--public: an s3 bucket with a public-read policy.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			t, err := resolveInfra(cmd, rt, client, args[0], envID)
			if err != nil {
				return err
			}
			if t.Kind != "instance" {
				return fmt.Errorf("%s is a slice; provision from an instance", t.Path)
			}
			s, err := client.ProvisionSlice(cmd.Context(), t.ID, args[1], rt.linkedEnv(envID), public)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(s)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Provisioned %s on %s\n  %s\n", s.Name, t.Path, s.Slug)
			return nil
		},
	}
	cmd.Flags().BoolVar(&public, "public", false, "s3: bucket with a public-read policy")
	cmd.Flags().StringVar(&envID, "env-id", "", "environment id (default: the linked env)")
	return cmd
}

func infraForkCmd(rt *Runtime) *cobra.Command {
	var name, envID string
	cmd := &cobra.Command{
		Use:   "fork <slice-path>",
		Short: "Copy a live slice into a fresh one beside it, data and all",
		Long: `Copy a live slice into a fresh one beside it, to rehearse a migration without
risking the original. The copy runs server-side and can take a while on a
large database; Ctrl-C stops waiting but the fork may complete anyway.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			t, err := resolveInfra(cmd, rt, client, args[0], envID)
			if err != nil {
				return err
			}
			if t.Kind != "slice" {
				return fmt.Errorf("%s is an instance; fork one of its slices", t.Path)
			}
			_, _ = fmt.Fprintf(rt.Stderr, "Forking %s. Copying data, this can take a while…\n", t.Path)
			s, err := client.ForkSlice(cmd.Context(), t.ID, name)
			if err != nil {
				if cmd.Context().Err() != nil {
					return fmt.Errorf("stopped waiting; the fork of %s may still complete server-side", t.Path)
				}
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(s)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Forked %s -> %s\n  %s\n", t.Name, s.Name, s.Slug)
			return nil
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "name for the fork (default: server picks)")
	cmd.Flags().StringVar(&envID, "env-id", "", "environment id for path resolution")
	return cmd
}

func infraLsCmd(rt *Runtime) *cobra.Command {
	var stack, envID string
	cmd := &cobra.Command{
		Use:     "ls [path]",
		Aliases: []string{"list"},
		Short:   "List instances, or one instance's slices",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if len(args) > 0 {
				t, err := resolveInfra(cmd, rt, client, args[0], envID)
				if err != nil {
					return err
				}
				if t.Kind != "instance" {
					return fmt.Errorf("%s is a slice; list the instance it sits on", t.Path)
				}
				slices, err := client.InstanceSlices(cmd.Context(), t.ID)
				if err != nil {
					return err
				}
				if rt.JSON {
					return rt.EmitJSON(slices)
				}
				cells := make([][]string, len(slices))
				for i, s := range slices {
					cells[i] = []string{s.Slug, s.Name, s.Status, orNone(strings.Join(s.Consumers, ","))}
				}
				rt.Table([]string{"SLUG", "NAME", "STATUS", "CONSUMERS"}, cells)
				return nil
			}
			dbs, err := client.DBs(cmd.Context())
			if err != nil {
				return err
			}
			out := make([]cli.DB, 0, len(dbs))
			for _, d := range dbs {
				if stack != "" && d.StackID != stack {
					continue
				}
				out = append(out, d)
			}
			if rt.JSON {
				return rt.EmitJSON(out)
			}
			cells := make([][]string, len(out))
			for i, d := range out {
				cells[i] = []string{d.ID, d.Engine, d.Name, orNone(d.Scope), d.Status, orNone(d.Path)}
			}
			rt.Table([]string{"ID", "ENGINE", "NAME", "SCOPE", "STATUS", "PATH"}, cells)
			return nil
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "filter instances by stack id")
	cmd.Flags().StringVar(&envID, "env-id", "", "environment id for path resolution")
	return cmd
}

func infraRmCmd(rt *Runtime) *cobra.Command {
	var envID string
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <path>",
		Aliases: []string{"delete"},
		Short:   "Remove an instance or a slice; says what goes with it",
		Long: `Remove an instance or a single slice; the path says which. An instance
takes every slice cut from it; a slice takes every consumer's link to it.
The prompt spells out that blast radius before anything happens.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			t, err := resolveInfra(cmd, rt, client, args[0], envID)
			if err != nil {
				return err
			}
			// --force is the legacy skip spelling; don't mutate rt.Yes, which
			// outlives this command in tests and legacy-forwarding trees.
			confirm := func(msg string) error {
				if force {
					return nil
				}
				return rt.Confirm(msg)
			}
			if t.Kind == "slice" {
				warn := ""
				if s := findSlice(cmd, client, t); len(s.Consumers) > 0 {
					warn = fmt.Sprintf(" Used by %s, so removing it unhooks %s.",
						strings.Join(s.Consumers, ", "), plural(len(s.Consumers), "that tile", "all of them"))
				}
				if err := confirm(fmt.Sprintf("Remove slice %s (%s)?%s", t.Path, t.Name, warn)); err != nil {
					return err
				}
				if err := client.DeleteSlice(cmd.Context(), t.ID); err != nil {
					return err
				}
			} else {
				warn := ""
				if slices, err := client.InstanceSlices(cmd.Context(), t.ID); err == nil && len(slices) > 0 {
					warn = fmt.Sprintf(" Destroys %d %s on it: %s.",
						len(slices), plural(len(slices), "slice", "slices"), sliceNames(slices))
				}
				if err := confirm(fmt.Sprintf("Remove instance %s?%s", t.Path, warn)); err != nil {
					return err
				}
				if err := client.DeleteDB(cmd.Context(), t.ID, true); err != nil {
					return err
				}
			}
			if rt.JSON {
				return rt.EmitJSON(removed{t.Path, true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed "+t.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&envID, "env-id", "", "environment id for path resolution")
	cmd.Flags().BoolVar(&force, "force", false, "skip the confirmation (same as --yes)")
	return cmd
}

// findSlice re-reads the resolved slice to learn its consumers. Resolution
// answers "what is this", not "what hangs off it".
func findSlice(cmd *cobra.Command, client *cli.Client, t cli.Target) cli.Slice {
	slices, err := client.InstanceSlices(cmd.Context(), t.InstanceID)
	if err != nil {
		return cli.Slice{}
	}
	for _, s := range slices {
		if s.ID == t.ID {
			return s
		}
	}
	return cli.Slice{}
}

func sliceNames(slices []cli.Slice) string {
	names := make([]string, 0, len(slices))
	for _, s := range slices {
		names = append(names, s.Slug)
	}
	return strings.Join(names, ", ")
}

func infraSetCmd(rt *Runtime) *cobra.Command {
	var envID, scope, image string
	var port, memory, shmSize int
	var cpu float64
	cmd := &cobra.Command{
		Use:   "set <path>",
		Short: "Change an instance's port, limits, scope or image",
		Long: `Change an instance's published port, resource limits, sharing scope, shm
size or image. Only flags actually passed are sent, an omitted field means
"leave it". --image "" reverts to the engine default.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			t, err := resolveInfra(cmd, rt, client, args[0], envID)
			if err != nil {
				return err
			}
			if t.Kind != "instance" {
				return fmt.Errorf("%s is a slice; it has no settings to change", t.Path)
			}
			var s cli.DBSettings
			if cmd.Flags().Changed("port") {
				s.ExternalPort = &port
			}
			if cmd.Flags().Changed("cpu") {
				s.CPULimit = &cpu
			}
			if cmd.Flags().Changed("memory") {
				s.MemLimitMB = &memory
			}
			if cmd.Flags().Changed("scope") {
				s.Scope = &scope
			}
			if cmd.Flags().Changed("image") {
				s.Image = &image
			}
			if cmd.Flags().Changed("shm-size") {
				s.ShmSizeMB = &shmSize
			}
			if s.ExternalPort == nil && s.CPULimit == nil && s.MemLimitMB == nil && s.Scope == nil && s.Image == nil && s.ShmSizeMB == nil {
				return usagef("nothing to set; pass --port, --cpu, --memory, --scope, --image or --shm-size")
			}
			d, err := client.PatchDB(cmd.Context(), t.ID, s)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Updated %s: port %d, scope %s, cpu %g, memory %dMB, image %s, shm %dMB\n",
				d.Name, d.ExternalPort, d.Scope, d.CPULimit, d.MemLimitMB, d.Image, d.ShmSizeMB)
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVar(&port, "port", 0, "published port")
	f.Float64Var(&cpu, "cpu", 0, "CPU limit in cores, e.g. 1.5")
	f.IntVar(&memory, "memory", 0, "memory limit in MB")
	f.StringVar(&scope, "scope", "", "sharing scope: env, stack or org")
	f.StringVar(&image, "image", "", `container image ("" reverts to the engine default)`)
	f.IntVar(&shmSize, "shm-size", 0, "shm size in MB")
	f.StringVar(&envID, "env-id", "", "environment id for path resolution")
	return cmd
}

func infraGetCmd(rt *Runtime) *cobra.Command {
	var envID string
	cmd := &cobra.Command{
		Use:     "get <path>",
		Aliases: []string{"show"},
		Short:   "Show one shared instance",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			t, err := resolveInfra(cmd, rt, client, args[0], envID)
			if err != nil {
				return err
			}
			if t.Kind != "instance" {
				return fmt.Errorf("%s is a slice; `infra ls` on the instance lists it", t.Path)
			}
			d, err := client.GetDB(cmd.Context(), t.ID)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			rt.Table([]string{"FIELD", "VALUE"}, [][]string{
				{"id", d.ID}, {"name", d.Name}, {"engine", d.Engine},
				{"scope", orNone(d.Scope)}, {"status", d.Status}, {"path", orNone(d.Path)},
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&envID, "env-id", "", "environment id for path resolution")
	return cmd
}

// infraPublicCmd exposes or hides a slice. Every consumer of the same bucket
// moves with it: one consumer public and its neighbour private is not a state
// the bucket can be in.
func infraPublicCmd(rt *Runtime) *cobra.Command {
	var envID string
	var off bool
	cmd := &cobra.Command{
		Use:   "public <slice-path>",
		Short: "Expose a slice outside its own network, or hide it again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			t, err := resolveInfra(cmd, rt, client, args[0], envID)
			if err != nil {
				return err
			}
			if t.Kind != "slice" {
				return fmt.Errorf("%s is an instance; public applies to a slice", t.Path)
			}
			s, err := client.SetSlicePublic(cmd.Context(), t.ID, !off)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(s)
			}
			state := "public"
			if off {
				state = "private"
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s is now %s\n", s.Slug, state)
			return nil
		},
	}
	cmd.Flags().StringVar(&envID, "env-id", "", "environment id for path resolution")
	cmd.Flags().BoolVar(&off, "off", false, "make it private again")
	return cmd
}
