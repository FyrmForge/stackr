package cmd

// The tile and deployment verbs that had no CLI: what the panel's buttons do,
// plus the read-only views a script needs to decide which button to press.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

// tileRef is the flag set every tile verb shares: a positional ref, or --tile,
// or the linked tile.
type tileRef struct {
	app, stack, env string
}

func (t *tileRef) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&t.app, "tile", "", "tile id (alternative to the ref argument)")
	f.StringVar(&t.stack, "stack", "", "stack id for ref resolution")
	f.StringVar(&t.env, "env", "", "env id for ref resolution")
}

func (t *tileRef) resolve(cmd *cobra.Command, rt *Runtime, client *cli.Client, args []string) (string, error) {
	return tileTarget(cmd, rt, client, args, t.app, t.stack, t.env)
}

// tileActionCmd builds the three verbs that differ only in which call they
// make: stop, restart, pause. Writing them out three times would be three
// copies of the same flag registration and ref resolution.
func tileActionCmd(rt *Runtime, use, short string, do func(*cli.Client, *cobra.Command, string) (cli.App, error)) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   use + " [ref]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			a, err := do(client, cmd, id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(tileRow{a.ID, a.Name, a.Status})
			}
			_, _ = fmt.Fprintf(rt.Stdout, "%s is now %s\n", a.Name, a.Status)
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

func tileGetCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   "get [ref]",
		Short: "Show one tile",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			a, err := client.GetApp(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(a)
			}
			rt.Table([]string{"FIELD", "VALUE"}, [][]string{
				{"id", a.ID}, {"name", a.Name}, {"status", a.Status},
			})
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

func tileRollbackCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	var tag string
	cmd := &cobra.Command{
		Use:   "rollback [ref]",
		Short: "Redeploy an image tag this tile has run before",
		Long: `Redeploy an image tag this tile has run before.

--tag is required: "the last good one" is a judgement, and guessing it here
would redeploy whatever happened to be second in the list. Use
` + "`stackr tile deployments`" + ` to pick one.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(tag) == "" {
				return usagef("--tag is required")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			dep, err := client.Rollback(cmd.Context(), id, tag)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(struct {
					Deployment string `json:"deployment"`
				}{dep})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Rolling back, deployment "+dep)
			return nil
		},
	}
	ref.register(cmd)
	cmd.Flags().StringVar(&tag, "tag", "", "image tag to roll back to (required)")
	return cmd
}

func tileDeploymentsCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   "deployments [ref]",
		Short: "List a tile's recent deployments",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			ds, err := client.Deployments(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(ds)
			}
			rows := make([][]string, len(ds))
			for i, d := range ds {
				rows[i] = []string{d.ID, d.Status, d.Trigger, shortSHA(d.CommitSHA), d.ImageTag}
			}
			rt.Table([]string{"ID", "STATUS", "TRIGGER", "COMMIT", "IMAGE"}, rows)
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

// shortSHA trims a commit to the length a person reads.
func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func tileMetricsCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	var window string
	cmd := &cobra.Command{
		Use:   "metrics [ref]",
		Short: "Show a tile's sampled cpu, memory and network use",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch window {
			case "1h", "6h", "24h":
			default:
				return usagef("--range must be 1h, 6h or 24h")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			ms, err := client.Metrics(cmd.Context(), id, window)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(ms)
			}
			rows := make([][]string, len(ms))
			for i, m := range ms {
				rows[i] = []string{m.At.Format("15:04:05"),
					strconv.FormatFloat(m.CPUPercent, 'f', 1, 64),
					strconv.FormatInt(m.MemBytes/(1024*1024), 10),
					strconv.FormatFloat(m.RxBps, 'f', 0, 64),
					strconv.FormatFloat(m.TxBps, 'f', 0, 64)}
			}
			rt.Table([]string{"AT", "CPU%", "MEM MB", "RX B/S", "TX B/S"}, rows)
			return nil
		},
	}
	ref.register(cmd)
	cmd.Flags().StringVar(&window, "range", "1h", "window: 1h, 6h or 24h")
	return cmd
}

func tileStopRunCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   "stop-run <run-id> [ref]",
		Short: "Stop a cron or function run that is still in flight",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args[1:])
			if err != nil {
				return err
			}
			if err := client.StopRun(cmd.Context(), id, args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Stopped run "+args[0])
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

func tileRefsCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   "refs [ref]",
		Short: "List what this tile can reference, and the expression for each",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			srcs, err := client.ReferenceCatalogue(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(srcs)
			}
			var rows [][]string
			for _, s := range srcs {
				for _, o := range s.Outputs {
					kind := o.Kind
					if o.Secret {
						kind += " (secret)"
					}
					rows = append(rows, []string{s.Expr(o.Name), kind})
				}
			}
			rt.Table([]string{"REFERENCE", "KIND"}, rows)
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

func tileResourcesCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   "resources [ref]",
		Short: "List the managed resources visible to a tile",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			rs, err := client.Resources(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(rs)
			}
			rows := make([][]string, len(rs))
			for i, r := range rs {
				bound := "no"
				if r.Bound {
					bound = "yes"
				}
				rows[i] = []string{r.Slug, r.Kind, r.Status, bound}
			}
			rt.Table([]string{"SLUG", "KIND", "STATUS", "ATTACHED"}, rows)
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

func tileDetachCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   "detach <provision-id> [ref]",
		Short: "Unhook a tile from a slice, keeping the data",
		Long: `Unhook a tile from a slice, keeping the data.

The slice itself and every other consumer of it are untouched. Dropping the
slice is ` + "`stackr infra rm`" + `.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args[1:])
			if err != nil {
				return err
			}
			if err := client.DetachProvision(cmd.Context(), id, args[0]); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{args[0], true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Detached "+args[0]+". The data was kept.")
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

func deploymentCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "deployment",
		Aliases: []string{"deployments"},
		Short:   "Inspect and cancel deployments",
	}
	cmd.AddCommand(deploymentGetCmd(rt), deploymentLogsCmd(rt), deploymentCancelCmd(rt))
	return cmd
}

func deploymentGetCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "get <deployment-id>",
		Short: "Show one deployment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			d, err := client.GetDeployment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			rt.Table([]string{"FIELD", "VALUE"}, [][]string{
				{"id", d.ID}, {"status", d.Status}, {"trigger", d.Trigger},
				{"commit", d.CommitSHA}, {"image", d.ImageTag}, {"error", orNone(d.Error)},
			})
			return nil
		},
	}
}

func deploymentLogsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "logs <deployment-id>",
		Short: "Print one deployment's build output",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			out, err := client.DeploymentLogs(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(struct {
					Lines string `json:"lines"`
				}{out})
			}
			_, _ = fmt.Fprintln(rt.Stdout, out)
			return nil
		},
	}
}

func deploymentCancelCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <deployment-id>",
		Short: "Cancel a build or roll-out in flight",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			d, err := client.CancelDeployment(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Deployment %s is now %s\n", d.ID, d.Status)
			return nil
		},
	}
}

func tileDomainAutoCmd(rt *Runtime) *cobra.Command {
	var ref tileRef
	cmd := &cobra.Command{
		Use:   "auto [ref]",
		Short: "Generate this tile's hostname under its environment's domain resource",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := ref.resolve(cmd, rt, client, args)
			if err != nil {
				return err
			}
			d, err := client.AutoDomain(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Added %s (%s)\n", d.Host, d.ID)
			return nil
		},
	}
	ref.register(cmd)
	return cmd
}

func tileDomainSetCmd(rt *Runtime) *cobra.Command {
	var httpsFlag bool
	var certFile, keyFile string
	var clearCert bool
	cmd := &cobra.Command{
		Use:   "set <domain-id>",
		Short: "Change a domain's TLS: HTTPS on or off, custom certificate in or out",
		Long: `Change a domain's TLS.

Only the flags you pass are sent. --cert and --key go together; --clear-cert
removes a custom certificate and falls back to the instance's resolver.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fields := map[string]any{}
			if cmd.Flags().Changed("https") {
				fields["https"] = httpsFlag
			}
			switch {
			case clearCert:
				fields["cert_pem"], fields["key_pem"] = "", ""
			case certFile != "" || keyFile != "":
				if certFile == "" || keyFile == "" {
					return usagef("--cert and --key must be given together")
				}
				cert, err := os.ReadFile(certFile)
				if err != nil {
					return err
				}
				key, err := os.ReadFile(keyFile)
				if err != nil {
					return err
				}
				fields["cert_pem"], fields["key_pem"] = string(cert), string(key)
			}
			if len(fields) == 0 {
				return usagef("nothing to set; pass --https, --cert/--key or --clear-cert")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			d, err := client.PatchDomain(cmd.Context(), args[0], fields)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Updated %s\n", d.Host)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&httpsFlag, "https", true, "serve this host over TLS")
	f.StringVar(&certFile, "cert", "", "PEM certificate chain file")
	f.StringVar(&keyFile, "key", "", "PEM private key file")
	f.BoolVar(&clearCert, "clear-cert", false, "remove the custom certificate")
	return cmd
}

func orgLsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the organizations this key can see",
		Long: `List the organizations this key can see.

The slug is what every path form starts with: acme, acme:shop,
acme:shop:prod, acme:shop:prod:api. Anywhere an id is taken, so is a path.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			orgs, err := client.Orgs(cmd.Context())
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(orgs)
			}
			rows := make([][]string, len(orgs))
			for i, o := range orgs {
				rows[i] = []string{o.Slug, o.Name, o.Role, o.ID}
			}
			rt.Table([]string{"SLUG", "NAME", "ROLE", "ID"}, rows)
			return nil
		},
	}
}

func stackRenameCmd(rt *Runtime) *cobra.Command {
	var stackFlag string
	cmd := &cobra.Command{
		Use:   "rename <new-name>",
		Short: "Rename a stack",
		Long: `Rename a stack.

The slug moves with the name, so every URL, CLI path and container name under
it moves too. A config-managed stack is refused: its file owns the name.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := rt.LinkedStack(stackFlag)
			if err != nil {
				return err
			}
			s, err := client.PatchStack(cmd.Context(), id, map[string]any{"name": args[0]})
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(s)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Renamed to %s\n", s.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	return cmd
}

func stackPREnvCmd(rt *Runtime) *cobra.Command {
	var stackFlag string
	var enable, disable, rotate bool
	var comment, status string
	cmd := &cobra.Command{
		Use:   "pr-envs",
		Short: "Show or change pull-request environment settings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := rt.LinkedStack(stackFlag)
			if err != nil {
				return err
			}
			cur, err := client.GetPREnv(cmd.Context(), id)
			if err != nil {
				return err
			}
			changed := enable || disable || rotate || comment != "" || status != ""
			if changed {
				fields := map[string]any{"enabled": cur.Enabled, "rotate_secret": rotate}
				if enable {
					fields["enabled"] = true
				}
				if disable {
					fields["enabled"] = false
				}
				if b, ok := parseTriState(comment); ok {
					fields["comment"] = b
				}
				if b, ok := parseTriState(status); ok {
					fields["status"] = b
				}
				cur, err = client.SetPREnv(cmd.Context(), id, fields)
				if err != nil {
					return err
				}
			}
			if rt.JSON {
				return rt.EmitJSON(cur)
			}
			rt.Table([]string{"FIELD", "VALUE"}, [][]string{
				{"enabled", yesNo(cur.Enabled)},
				{"comment on PR", yesNo(cur.Comment)},
				{"commit status", yesNo(cur.Status)},
				{"webhook secret set", yesNo(cur.SecretSet)},
			})
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&stackFlag, "stack", "", "stack id or org:stack path (default: the linked stack)")
	f.BoolVar(&enable, "enable", false, "build an environment per pull request")
	f.BoolVar(&disable, "disable", false, "stop building them")
	f.StringVar(&comment, "comment", "", "post a comment on the PR with its URLs: on or off")
	f.StringVar(&status, "status", "", "report a commit status on the PR: on or off")
	f.BoolVar(&rotate, "rotate-secret", false, "mint a new webhook secret; the old one stops validating at once")
	return cmd
}

// parseTriState reads an on/off flag that also has an unset state, so a caller
// changing one knob does not silently reset the other.
func parseTriState(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "on", "true", "yes", "1":
		return true, true
	case "off", "false", "no", "0":
		return false, true
	}
	return false, false
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func storageProbeCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "probe <storage-id>",
		Short: "Mount the share on the node that will use it and record the result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			st, err := client.ProbeStorage(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(st)
			}
			if st.Status == "ok" {
				_, _ = fmt.Fprintln(rt.Stdout, "Probe OK.")
				return nil
			}
			return fmt.Errorf("probe failed: %s", st.StatusMsg)
		},
	}
}
