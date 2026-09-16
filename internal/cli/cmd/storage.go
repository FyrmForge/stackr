package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

func newStorageCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Manage server-scoped shares/pools and their declared paths",
		Long: `Manage storage: NFS/SMB/local shares tiles can mount. Attach a declared
path to a tile with ` + "`stackr tile set --storage slug/subpath:/mount[:ro]`" + `.`,
	}
	cmd.AddCommand(storageLsCmd(rt), storageAddCmd(rt), storageProbeCmd(rt), storageRmCmd(rt), storagePathCmd(rt))
	return cmd
}

func storageLsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List storages and their declared paths",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			es, err := client.ListStorage(cmd.Context())
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(es)
			}
			for _, e := range es {
				org := e.Org
				if org == "" {
					org = "-"
				}
				_, _ = fmt.Fprintf(rt.Stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Slug, org, e.Backend, e.Status, e.StatusMsg)
				for _, p := range e.Paths {
					ro := ""
					if p.ForcedRO {
						ro = "\tforced-ro"
					}
					_, _ = fmt.Fprintf(rt.Stdout, "  %s\t%s/%s\t/%s\t%s%s\n", p.ID, e.Slug, p.Name, p.Subpath, p.Volume, ro)
				}
			}
			return nil
		},
	}
}

func storageAddCmd(rt *Runtime) *cobra.Command {
	var in cli.StorageCreate
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a storage (the server probes it before saving)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name = args[0]
			if in.Password == "" {
				// Never require the secret on argv: env var, then a masked
				// prompt on a TTY.
				if v := rt.Getenv("STACKR_STORAGE_PASSWORD"); v != "" {
					in.Password = v
				} else if in.Username != "" {
					pw, err := rt.PromptSecret("Password for " + in.Username + ": ")
					if err != nil {
						return err
					}
					in.Password = pw
				}
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			e, err := client.CreateStorage(cmd.Context(), in)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(e)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Created %s (%s). Probe %s %s\n", e.Slug, e.Backend, e.Status, e.StatusMsg)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Backend, "backend", "", "backend: nfs, smb or local")
	f.StringVar(&in.Address, "address", "", "server host")
	f.StringVar(&in.Export, "export", "", "export/share path")
	f.StringVar(&in.Username, "username", "", "username")
	f.StringVar(&in.Password, "password", "", "password (prefer STACKR_STORAGE_PASSWORD or the prompt)")
	f.StringVar(&in.Opts, "opts", "", "raw mount options")
	f.StringVar(&in.Org, "org", "", "org slug: an org share (nfs/smb), mounted as ${{ org.storage.NAME }}")
	return cmd
}

func storageRmCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm <id>",
		Aliases: []string{"delete"},
		Short:   "Remove a storage row (data on the share/pool is untouched)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.Confirm(fmt.Sprintf("Remove storage %s? Data on the share/pool is untouched.", args[0])); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteStorage(cmd.Context(), args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed. Data on the share/pool is untouched.")
			return nil
		},
	}
	return cmd
}

func storagePathCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path",
		Short: "Manage a storage's declared sub-paths",
	}
	var subpath string
	var ro bool
	add := &cobra.Command{
		Use:   "add <storage-id> <name>",
		Short: "Declare a sub-path",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			p, err := client.CreateStoragePath(cmd.Context(), args[0], cli.StoragePathCreate{
				Name: args[1], Subpath: subpath, ForcedRO: ro,
			})
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(p)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Declared %s (volume %s)\n", p.Name, p.Volume)
			return nil
		},
	}
	add.Flags().StringVar(&subpath, "subpath", "", "sub-path inside the share")
	add.Flags().BoolVar(&ro, "ro", false, "force read-only")
	rm := &cobra.Command{
		Use:   "rm <path-id>",
		Short: "Remove a declared sub-path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.Confirm(fmt.Sprintf("Remove storage path %s?", args[0])); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteStoragePath(cmd.Context(), args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Sub-path removed.")
			return nil
		},
	}
	cmd.AddCommand(add, rm)
	return cmd
}
