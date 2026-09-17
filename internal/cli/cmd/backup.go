package cmd

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

// backupTarget resolves the tile a schedule hangs off. Shared instances are
// not in /apps, so resolveTileID alone cannot name one, and a database
// instance is the commonest backup subject there is.
func backupTarget(cmd *cobra.Command, rt *Runtime, client *cli.Client, ref, appFlag, stack, env string) (string, error) {
	if ref == "" {
		return rt.resolveAppID(appFlag)
	}
	if dbs, err := client.DBs(cmd.Context()); err == nil {
		link, _, _ := rt.Link()
		linkStack := stack
		if linkStack == "" {
			linkStack = link.Stack
		}
		for _, d := range dbs {
			// A full id was typed on purpose; a bare name only matches within
			// the linked (or --stack) stack, /dbs is not stack-scoped.
			if d.ID == ref || (d.Name == ref && (linkStack == "" || d.StackID == linkStack)) {
				return d.ID, nil
			}
		}
	}
	return resolveTileID(cmd.Context(), rt, client, ref, stack, env)
}

func newBackupCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "backup",
		Aliases: []string{"backups"},
		Short:   "Backup schedules, their runs, and S3 destinations",
	}
	cmd.AddCommand(
		backupLsCmd(rt), backupCreateCmd(rt), backupSetCmd(rt), backupRmCmd(rt),
		backupRunsCmd(rt), backupRunCmd(rt), backupRestoreCmd(rt), backupDestCmd(rt),
	)
	return cmd
}

type backupRow struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Cron          string `json:"cron"`
	DestinationID string `json:"destination_id"`
	KeepLatest    int    `json:"keep_latest"`
	Enabled       bool   `json:"enabled"`
}

func backupLsCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env string
	cmd := &cobra.Command{
		Use:     "ls [tile]",
		Aliases: []string{"list"},
		Short:   "List a tile's backup schedules",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			tile, err := backupTarget(cmd, rt, client, ref, appFlag, stack, env)
			if err != nil {
				return err
			}
			bs, err := client.Backups(cmd.Context(), tile)
			if err != nil {
				return err
			}
			rows := make([]backupRow, 0, len(bs))
			for _, b := range bs {
				rows = append(rows, backupRow{b.ID, b.Kind, b.Cron, b.DestinationID, b.KeepLatest, b.Enabled})
			}
			if rt.JSON {
				return rt.EmitJSON(rows)
			}
			cells := make([][]string, len(rows))
			for i, b := range rows {
				// keep=/enabled= prefixes match the old CLI's piped shape
				cells[i] = []string{b.ID, b.Kind, b.Cron, b.DestinationID, "keep=" + keepLabel(b.KeepLatest), fmt.Sprintf("enabled=%v", b.Enabled)}
			}
			rt.Table([]string{"ID", "KIND", "CRON", "DEST", "KEEP", "ENABLED"}, cells)
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id for ref resolution")
	cmd.Flags().StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func backupCreateCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env, dest, schedule, kind, mode, tz string
	var keep int
	var disabled bool
	cmd := &cobra.Command{
		Use:   "create [tile]",
		Short: "Add a backup schedule",
		Long: `Add a schedule. kind and mode default off the tile (dump for a database,
volume otherwise; mode pause). --keep 0 keeps every archive.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dest == "" || schedule == "" {
				return usagef("--dest and --schedule are required")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			tile, err := backupTarget(cmd, rt, client, ref, appFlag, stack, env)
			if err != nil {
				return err
			}
			fields := map[string]any{"destination_id": dest, "cron": schedule}
			if kind != "" {
				fields["kind"] = kind
			}
			if mode != "" {
				fields["container_mode"] = mode
			}
			if tz != "" {
				fields["timezone"] = tz
			}
			if cmd.Flags().Changed("keep") {
				fields["keep_latest"] = keep
			}
			if disabled {
				fields["enabled"] = false
			}
			b, err := client.CreateBackup(cmd.Context(), tile, fields)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(b)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Created %s: %s on %s → %s\n", b.ID, b.Kind, b.Cron, b.DestinationID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dest, "dest", "", "destination id, or ${{ org.backups.NAME }} (required)")
	f.StringVar(&schedule, "schedule", "", "cron expression (required)")
	f.StringVar(&kind, "kind", "", "dump or volume (default: from the tile)")
	f.StringVar(&mode, "mode", "", "container mode: pause, stop or live (default pause)")
	f.StringVar(&tz, "tz", "", "timezone for the schedule")
	f.IntVar(&keep, "keep", 0, "archives to keep (0 = all)")
	f.BoolVar(&disabled, "disabled", false, "create disabled")
	f.StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	f.StringVar(&stack, "stack", "", "stack id for ref resolution")
	f.StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func backupSetCmd(rt *Runtime) *cobra.Command {
	var dest, schedule, mode, tz, enabled string
	var keep int
	cmd := &cobra.Command{
		Use:   "set <backup-id>",
		Short: "Change a schedule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fields := map[string]any{}
			for _, kv := range []struct{ v, key string }{
				{dest, "destination_id"}, {schedule, "cron"}, {mode, "container_mode"}, {tz, "timezone"},
			} {
				if kv.v != "" {
					fields[kv.key] = kv.v
				}
			}
			if cmd.Flags().Changed("keep") {
				fields["keep_latest"] = keep
			}
			if enabled != "" {
				// String-typed on purpose: `--enabled true` (spaced value) is
				// the compatible spelling, but validate rather than reading
				// anything ≠ "true" as false.
				v, err := strconv.ParseBool(enabled)
				if err != nil {
					return usagef("--enabled must be true or false")
				}
				fields["enabled"] = v
			}
			if len(fields) == 0 {
				return usagef("nothing to set; pass --dest, --schedule, --mode, --tz, --keep or --enabled")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			b, err := client.PatchBackup(cmd.Context(), args[0], fields)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(b)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Updated %s (%s, enabled=%v)\n", b.ID, b.Cron, b.Enabled)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dest, "dest", "", "destination id, or ${{ org.backups.NAME }}")
	f.StringVar(&schedule, "schedule", "", "cron expression")
	f.StringVar(&mode, "mode", "", "container mode: pause, stop or live")
	f.StringVar(&tz, "tz", "", "timezone")
	f.IntVar(&keep, "keep", 0, "archives to keep (0 = all)")
	f.StringVar(&enabled, "enabled", "", "true or false")
	return cmd
}

func backupRmCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <backup-id>",
		Aliases: []string{"delete"},
		Short:   "Remove a schedule (archives are kept)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.Confirm(fmt.Sprintf("Remove backup schedule %s? Archives already in the bucket are kept.", args[0])); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteBackup(cmd.Context(), args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed "+args[0]+" (archives already in the bucket are kept).")
			return nil
		},
	}
}

func backupRunsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "runs <backup-id>",
		Short: "Run history: where a restore's --run id comes from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			rs, err := client.BackupRuns(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(rs)
			}
			cells := make([][]string, len(rs))
			for i, r := range rs {
				// A failed run has no object key, so the last column is
				// whichever of the two says what happened.
				detail := r.ObjectKey
				if r.Error != "" {
					detail = r.Error
				}
				cells[i] = []string{r.ID, r.Status, r.Trigger, r.CreatedAt, humanSize(r.SizeBytes), orNone(detail)}
			}
			rt.Table([]string{"ID", "STATUS", "TRIGGER", "CREATED", "SIZE", "OBJECT/ERROR"}, cells)
			return nil
		},
	}
}

func backupRunCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "run <backup-id>",
		Short: "Run a backup now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			r, err := client.RunBackup(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(r)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Started run %s (%s)\n", r.ID, r.Status)
			return nil
		},
	}
}

func backupRestoreCmd(rt *Runtime) *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:   "restore <backup-id>",
		Short: "Write an archive back over the live data",
		Long: `Restore a run's archive over the live database or volume. Nothing snapshots
what it replaces. This cannot be undone. The restore runs server-side to
completion even if you Ctrl-C the wait.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if runID == "" {
				return usagef("--run <run-id> is required (`stackr backup runs %s` lists them)", args[0])
			}
			if err := rt.Confirm(fmt.Sprintf("Restore run %s over the live data? This cannot be undone.", runID)); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			st, err := client.Restore(cmd.Context(), args[0], runID)
			if err != nil {
				return err
			}
			// Queued server-side; wait on the same item so "Restored." still
			// means restored.
			for {
				switch st.Status {
				case "done":
				case "error":
					return fmt.Errorf("restore failed: %s", st.Error)
				case "cancelled", "superseded":
					return fmt.Errorf("restore %s", st.Status)
				default:
					select {
					case <-cmd.Context().Done():
						return fmt.Errorf("stopped waiting; the restore continues server-side")
					case <-time.After(2 * time.Second):
					}
					if st, err = client.LatestRestore(cmd.Context(), args[0]); err != nil {
						return err
					}
					continue
				}
				break
			}
			if rt.JSON {
				return rt.EmitJSON(struct {
					BackupID string `json:"backup_id"`
					RunID    string `json:"run_id"`
					Restored bool   `json:"restored"`
				}{args[0], runID, true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Restored.")
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "run id to restore (required)")
	return cmd
}

// ---- backup dest ----

func backupDestCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dest",
		Short: "S3 destinations backups are written to",
		Args:  cobra.NoArgs,
		// bare `backup dest` listed destinations
		RunE: func(cmd *cobra.Command, args []string) error { return runBackupDestLs(cmd, rt) },
	}
	cmd.AddCommand(backupDestLsCmd(rt), backupDestAddCmd(rt), backupDestSetCmd(rt), backupDestRmCmd(rt))
	return cmd
}

func backupDestLsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List backup destinations",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, args []string) error { return runBackupDestLs(cmd, rt) },
	}
}

func runBackupDestLs(cmd *cobra.Command, rt *Runtime) error {
	client, err := rt.Client()
	if err != nil {
		return err
	}
	ds, err := client.Destinations(cmd.Context())
	if err != nil {
		return err
	}
	if rt.JSON {
		return rt.EmitJSON(ds)
	}
	cells := make([][]string, len(ds))
	for i, d := range ds {
		scope := "org:" + d.OrgID
		if d.Global {
			scope = "server-wide"
			if d.Shared {
				scope += " (shared)"
			}
		}
		cells[i] = []string{d.ID, d.Name, d.Endpoint + "/" + d.Bucket, scope}
	}
	rt.Table([]string{"ID", "NAME", "ENDPOINT/BUCKET", "SCOPE"}, cells)
	return nil
}

func backupDestAddCmd(rt *Runtime) *cobra.Command {
	var in cli.DestinationCreate
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add an S3 destination (keys prompted when not passed)",
		Long: `Add an S3 destination. The server dials the bucket before storing it, so
bad credentials fail here. Keys are prompted (masked) rather than required as
flags, because argv leaks into shell history and ps; env vars
STACKR_BACKUP_ACCESS_KEY / STACKR_BACKUP_SECRET_KEY also work.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name = args[0]
			if in.Endpoint == "" || in.Bucket == "" {
				return usagef("--endpoint and --bucket are required")
			}
			var err error
			if in.AccessKey == "" {
				in.AccessKey = rt.Getenv("STACKR_BACKUP_ACCESS_KEY")
			}
			if in.SecretKey == "" {
				in.SecretKey = rt.Getenv("STACKR_BACKUP_SECRET_KEY")
			}
			if in.AccessKey == "" {
				if in.AccessKey, err = rt.PromptSecret("Access key: "); err != nil {
					return err
				}
			}
			if in.SecretKey == "" {
				if in.SecretKey, err = rt.PromptSecret("Secret key: "); err != nil {
					return err
				}
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			d, err := client.AddDestination(cmd.Context(), in)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Added %s (%s)\n", d.Name, d.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Endpoint, "endpoint", "", "S3 endpoint URL (required)")
	f.StringVar(&in.Bucket, "bucket", "", "bucket name (required)")
	f.StringVar(&in.Region, "region", "", "region")
	f.StringVar(&in.OrgID, "org", "", "org id (empty = server-wide, admin only)")
	f.BoolVar(&in.Shared, "shared", false, "server-wide only: offer it to every organization (the bucket credentials go with it)")
	f.StringVar(&in.AccessKey, "access-key", "", "access key (prefer the prompt or env var)")
	f.StringVar(&in.SecretKey, "secret-key", "", "secret key (prefer the prompt or env var)")
	return cmd
}

func backupDestRmCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>",
		Aliases: []string{"delete"},
		Short:   "Remove a destination (archives in the bucket stay)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.Confirm(fmt.Sprintf("Remove backup destination %s? Archives in the bucket stay.", args[0])); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteDestination(cmd.Context(), args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed.")
			return nil
		},
	}
}

// backupDestSetCmd flips the shared toggle on a server-wide destination.
// Nothing else is settable: endpoint, bucket and credentials are what the
// archives already written were written with, so changing them in place would
// silently orphan them.
func backupDestSetCmd(rt *Runtime) *cobra.Command {
	var shared bool
	cmd := &cobra.Command{
		Use:   "set <id>",
		Short: "Share a server-wide destination with every organization, or take it back",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("shared") {
				return usagef("pass --shared or --shared=false")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			d, err := client.SetDestinationShared(cmd.Context(), args[0], shared)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			state := "no longer shared"
			if d.Shared {
				state = "shared with every organization"
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s is %s\n", d.Name, state)
			return nil
		},
	}
	cmd.Flags().BoolVar(&shared, "shared", true, "offer it to every organization")
	return cmd
}
