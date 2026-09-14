package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/cli"
)

type tileRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// tileTarget resolves the tile most commands act on: the positional ref if
// given, else --tile, else the link.
func tileTarget(cmd *cobra.Command, rt *Runtime, client *cli.Client, args []string, appFlag, stack, env string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return resolveTileID(cmd.Context(), rt, client, args[0], stack, env)
	}
	return rt.resolveAppID(appFlag)
}

func newTileCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tile",
		Aliases: []string{"apps"}, // back-compat; `apps` predates the tile noun
		Short:   "Manage tiles (services, crons, functions)",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, args []string) error { return runTileLs(cmd, rt) },
	}
	cmd.AddCommand(
		tileLsCmd(rt), tileGetCmd(rt), tileCreateCmd(rt), tileSetCmd(rt), tileRmCmd(rt),
		tileRunCmd(rt), tileAttachCmd(rt), tileProvisionsCmd(rt), tileDetachCmd(rt),
		tileDomainCmd(rt), tileVolumeCmd(rt),
		tileActionCmd(rt, "stop", "Scale a tile to zero, keeping its row and volumes",
			func(cl *cli.Client, cmd *cobra.Command, id string) (cli.App, error) {
				return cl.StopApp(cmd.Context(), id)
			}),
		tileActionCmd(rt, "restart", "Bounce a tile in place: no rebuild, no new image",
			func(cl *cli.Client, cmd *cobra.Command, id string) (cli.App, error) {
				return cl.RestartApp(cmd.Context(), id)
			}),
		tileActionCmd(rt, "pause", "Pause a cron schedule, or resume a paused one",
			func(cl *cli.Client, cmd *cobra.Command, id string) (cli.App, error) {
				return cl.ToggleCron(cmd.Context(), id)
			}),
		tileRollbackCmd(rt), tileDeploymentsCmd(rt), tileMetricsCmd(rt),
		tileStopRunCmd(rt), tileRefsCmd(rt), tileResourcesCmd(rt),
	)
	return cmd
}

func tileLsCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List tiles in the linked stack/env",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runTileLs(cmd, rt) },
	}
}

func runTileLs(cmd *cobra.Command, rt *Runtime) error {
	client, err := rt.Client()
	if err != nil {
		return err
	}
	link, _, _ := rt.Link()
	apps, err := client.Apps(cmd.Context(), link.Stack, link.Env)
	if err != nil {
		return err
	}
	rows := make([]tileRow, 0, len(apps))
	for _, a := range apps {
		rows = append(rows, tileRow{a.ID, a.Name, a.Status})
	}
	if rt.JSON {
		return rt.EmitJSON(rows)
	}
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = []string{r.ID, r.Name, r.Status}
	}
	rt.Table([]string{"ID", "NAME", "STATUS"}, cells)
	return nil
}

func tileCreateCmd(rt *Runtime) *cobra.Command {
	var (
		in                 cli.AppCreate
		stack              string
		isCron, isFunction bool
		envKV              []string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a tile",
		Long: `Create a tile: a service (default), or with --cron / --function a scheduled
or one-shot tile. Source is --git or --image; --connector names the git
connector for private repos.`,
		Example: `  stackr tile create web --git https://github.com/acme/web --port 3000
  stackr tile create nightly --cron --schedule "0 3 * * *" --command "rake cleanup"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name = args[0]
			in.Env = map[string]string{}
			for _, pair := range envKV {
				if k, v, ok := strings.Cut(pair, "="); ok {
					in.Env[k] = v
				}
			}
			if len(in.Env) == 0 {
				in.Env = nil
			}
			if isCron {
				in.Kind = "cron"
			}
			if isFunction {
				in.Kind = "function"
			}
			if in.Kind == "" {
				// schedule/command/timeout only apply to cron/function tiles
				in.Schedule, in.Command, in.TimeoutMinutes = "", "", 0
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			stackID, err := rt.LinkedStack(stack)
			if err != nil {
				return err
			}
			a, err := client.CreateApp(cmd.Context(), stackID, in)
			if err != nil {
				return err
			}
			kind := in.Kind
			if kind == "" {
				kind = "service"
			}
			if rt.JSON {
				return rt.EmitJSON(tileRow{a.ID, a.Name, a.Status})
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Created %s %s (%s)\n", kind, a.Name, a.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.EnvSlug, "env-slug", "", "environment slug to create in")
	f.StringVar(&in.Image, "image", "", "container image ref")
	f.StringVar(&in.GitURL, "git", "", "git repository URL")
	f.StringVar(&in.GitBranch, "branch", "", "git branch")
	f.StringVar(&in.DockerfilePath, "dockerfile", "", "Dockerfile path in the repo")
	f.StringVar(&in.BuildContext, "build-context", "", "build context directory")
	f.IntVar(&in.Port, "port", 0, "container port")
	f.StringArrayVar(&envKV, "env", nil, "environment variable K=V (repeatable)")
	f.StringVar(&in.Connector, "connector", "", "git connector id")
	f.StringVar(&stack, "stack", "", "stack id (default: the linked stack)")
	f.BoolVar(&isCron, "cron", false, "create a cron tile")
	f.BoolVar(&isFunction, "function", false, "create a function tile")
	f.StringVar(&in.Schedule, "schedule", "", "cron schedule expression")
	f.StringVar(&in.Command, "command", "", "command to run (cron/function)")
	f.IntVar(&in.TimeoutMinutes, "timeout", 0, "run timeout in minutes (cron/function)")
	return cmd
}

// tileSetFlags maps a CLI flag to the PATCH body key it writes and how to
// parse its value. Boolean-valued fields stay string-typed on purpose:
// today's syntax is `--push-registry true` (spaced value) and stays valid.
var tileSetFlags = []struct {
	flag  string
	key   string
	usage string
	parse func(string) (any, error)
}{
	{"image", "image", "container image ref", asString},
	{"git", "git_url", "git repository URL", asString},
	{"branch", "branch", "git branch", asString},
	{"connector", "connector", "git connector id", asString},
	{"port", "port", "container port", asInt},
	{"healthcheck", "healthcheck", "healthcheck command", asString},
	{"build-args", "build_args", "build args", asString},
	{"published-ports", "published_ports", "published ports spec", asString},
	{"traefik-override", "traefik_override", "traefik override YAML", asString},
	{"sec-headers", "security_headers", "security headers (true|false)", asBool},
	{"watch-paths", "watch_paths", "comma-separated watch paths", asLines},
	{"schedule", "schedule", "cron schedule expression", asString},
	{"command", "command", "command to run", asString},
	{"timeout", "timeout_minutes", "run timeout in minutes", asInt},
	{"allow-overlap", "allow_overlap", "allow overlapping runs (true|false)", asBool},
	{"user", "user", "UID[:GID]", asString},
	{"shm-size", "shm_size_mb", "shm size in MB", asInt},
	{"privileged", "privileged", "privileged container (true|false)", asBool},
	{"devices", "devices", "comma-separated host[:ctr[:perms]]", asLines},
	{"restart", "restart", "restart policy: always|on-failure|no", asString},
	{"depends-on", "depends_on", "comma-separated slug[:started|healthy|completed]", asLines},
	{"files", "files", "comma-separated repo/path:/ctr/path[:template]", asLines},
	{"storage", "storage", "comma-separated slug/subpath:/mount[:ro]", asLines},
	{"update-policy", "update_policy", "off|notify|auto", asString},
	{"wait-for-ci", "wait_for_ci", "wait for CI before deploying (true|false)", asBool},
}

func asString(s string) (any, error) { return s, nil }
func asInt(s string) (any, error)    { return strconv.Atoi(s) }
func asBool(s string) (any, error)   { return strconv.ParseBool(s) }

// asLines splits a comma-separated list; watch_paths is a list on the wire
// even though the tile stores it newline-joined.
func asLines(s string) (any, error) {
	if s == "" {
		return []string{}, nil
	}
	return strings.Split(s, ","), nil
}

func tileSetCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env, dockerfile, buildContext, cpu, memory string
	cmd := &cobra.Command{
		Use:   "set [ref]",
		Short: "Change tile settings",
		Long: `Change tile settings. Only flags actually passed are sent; the API treats
a present key as "set this", including to zero or empty.

--dockerfile/--build-context are a coupled pair (sending one clears the
other), as are --cpu/--memory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fields := map[string]any{}
			for _, spec := range tileSetFlags {
				fl := cmd.Flags().Lookup(spec.flag)
				if !fl.Changed {
					continue
				}
				v, err := spec.parse(fl.Value.String())
				if err != nil {
					return usagef("--%s: %v", spec.flag, err)
				}
				fields[spec.key] = v
			}
			// build is nested, so its two halves are read together, sending
			// only one would clear the other.
			if cmd.Flags().Changed("dockerfile") || cmd.Flags().Changed("build-context") {
				fields["build"] = map[string]string{
					"dockerfile": dockerfile,
					"context":    buildContext,
				}
			}
			if cmd.Flags().Changed("cpu") || cmd.Flags().Changed("memory") {
				cpuV, err := strconv.ParseFloat(orZero(cpu), 64)
				if err != nil {
					return usagef("--cpu must be a number of cores, e.g. 1.5")
				}
				memV, err := strconv.Atoi(orZero(memory))
				if err != nil {
					return usagef("--memory must be a whole number of MB")
				}
				fields["limits"] = map[string]any{"cpu": cpuV, "memory_mb": memV}
			}
			if len(fields) == 0 {
				return usagef("nothing to set; see `stackr tile set --help` for the field flags")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := tileTarget(cmd, rt, client, args, appFlag, stack, env)
			if err != nil {
				return err
			}
			a, err := client.PatchApp(cmd.Context(), id, fields)
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if rt.JSON {
				return rt.EmitJSON(struct {
					ID      string   `json:"id"`
					Name    string   `json:"name"`
					Updated []string `json:"updated"`
				}{a.ID, a.Name, keys})
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Updated %s: %s\n", a.Name, strings.Join(keys, ", "))
			return nil
		},
	}
	f := cmd.Flags()
	for _, spec := range tileSetFlags {
		f.String(spec.flag, "", spec.usage)
	}
	f.StringVar(&dockerfile, "dockerfile", "", "Dockerfile path (coupled with --build-context)")
	f.StringVar(&buildContext, "build-context", "", "build context dir (coupled with --dockerfile)")
	f.StringVar(&cpu, "cpu", "", "CPU limit in cores, e.g. 1.5 (coupled with --memory)")
	f.StringVar(&memory, "memory", "", "memory limit in MB (coupled with --cpu)")
	f.StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	f.StringVar(&stack, "stack", "", "stack id for ref resolution")
	f.StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func tileRmCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env string
	cmd := &cobra.Command{
		Use:   "rm [ref]",
		Short: "Remove a tile: its containers, route and row",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := tileTarget(cmd, rt, client, args, appFlag, stack, env)
			if err != nil {
				return err
			}
			if err := rt.Confirm(fmt.Sprintf("Remove tile %s, its containers, route and row?", id)); err != nil {
				return err
			}
			if err := client.DeleteApp(cmd.Context(), id); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{id, true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed "+id)
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id for ref resolution")
	cmd.Flags().StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func tileRunCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env string
	cmd := &cobra.Command{
		Use:   "run [ref]",
		Short: "Fire one immediate run of a cron or function tile",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := tileTarget(cmd, rt, client, args, appFlag, stack, env)
			if err != nil {
				return err
			}
			if err := client.RunApp(cmd.Context(), id); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(struct {
					ID      string `json:"id"`
					Started bool   `json:"started"`
				}{id, true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Run started. Watch it under the tile's Runs tab or `stackr tile "+id+" …`")
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id for ref resolution")
	cmd.Flags().StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func tileAttachCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env, provisionID, envVar, envID string
	cmd := &cobra.Command{
		Use:   "attach [ref] <slice-path>",
		Short: "Let a tile consume an existing slice (same env)",
		Long: `Bind an existing slice to a tile. Creating the slice is
` + "`stackr infra provision`" + `. Which tile consumes it is a property of the tile.`,
		Args: cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			// Split args: with 2, args[0] is the tile ref; with 1 it is the
			// slice path (tile from --tile/link); with 0, --provision names it.
			var ref, path string
			switch len(args) {
			case 2:
				ref, path = args[0], args[1]
			case 1:
				path = args[0]
			}
			pid := provisionID
			if pid == "" {
				if path == "" {
					return usagef("usage: stackr tile attach [ref] <slice-path> [--var ENV_NAME]")
				}
				t, err := client.Resolve(cmd.Context(), path, rt.linkedEnv(envID))
				if err != nil {
					return err
				}
				if t.Kind != "slice" {
					return fmt.Errorf("%s is an instance; attach a slice, or cut one with `stackr infra provision`", t.Path)
				}
				pid = t.ID
			}
			var id string
			if ref != "" {
				id, err = resolveTileID(cmd.Context(), rt, client, ref, stack, env)
			} else {
				id, err = rt.resolveAppID(appFlag)
			}
			if err != nil {
				return err
			}
			p, err := client.AttachProvision(cmd.Context(), id, pid, envVar)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(p)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Attached db %s (secret %s)\n", p.DBName, p.Secret)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&provisionID, "provision", "", "slice id (bypasses path resolution; what the panel's copy button gives)")
	f.StringVar(&envVar, "var", "", "env var name to wire the connection secret into")
	f.StringVar(&envID, "env-id", "", "environment id for relative path resolution")
	f.StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	f.StringVar(&stack, "stack", "", "stack id for ref resolution")
	f.StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func tileProvisionsCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env string
	cmd := &cobra.Command{
		Use:   "provisions [ref]",
		Short: "List the slices a tile holds",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := tileTarget(cmd, rt, client, args, appFlag, stack, env)
			if err != nil {
				return err
			}
			ps, err := client.ListProvisions(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(ps)
			}
			cells := make([][]string, len(ps))
			for i, p := range ps {
				cells[i] = []string{p.ID, p.DBName, p.Secret, p.Status}
			}
			rt.Table([]string{"ID", "DB", "SECRET", "STATUS"}, cells)
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id for ref resolution")
	cmd.Flags().StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

// ---- tile domain ----

func tileDomainCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domain",
		Short: "Manage a tile's attached hosts",
	}
	cmd.AddCommand(tileDomainAddCmd(rt), tileDomainAutoCmd(rt), tileDomainLsCmd(rt),
		tileDomainSetCmd(rt), tileDomainRmCmd(rt))
	return cmd
}

func tileDomainAddCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env string
	var port int
	var noHTTPS bool
	cmd := &cobra.Command{
		Use:   "add [ref] <host>",
		Short: "Attach a host to a tile (updates the proxy)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			var id, host string
			if len(args) == 2 {
				id, err = resolveTileID(cmd.Context(), rt, client, args[0], stack, env)
				host = args[1]
			} else {
				id, err = rt.resolveAppID(appFlag)
				host = args[0]
			}
			if err != nil {
				return err
			}
			d, err := client.AddDomain(cmd.Context(), id, host, port, !noHTTPS)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(d)
			}
			scheme := "http"
			if d.HTTPS {
				scheme = "https"
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Added %s://%s (%s)\n", scheme, d.Host, d.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVar(&port, "port", 0, "container port (0 = the tile's own)")
	f.BoolVar(&noHTTPS, "no-https", false, "serve plain HTTP")
	f.StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	f.StringVar(&stack, "stack", "", "stack id for ref resolution")
	f.StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func tileDomainLsCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env string
	cmd := &cobra.Command{
		Use:     "ls [ref]",
		Aliases: []string{"list"},
		Short:   "List a tile's hosts",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := tileTarget(cmd, rt, client, args, appFlag, stack, env)
			if err != nil {
				return err
			}
			ds, err := client.Domains(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(ds)
			}
			cells := make([][]string, len(ds))
			for i, d := range ds {
				cells[i] = []string{d.ID, d.Host, fmt.Sprintf(":%d", d.ContainerPort), fmt.Sprintf("https=%v", d.HTTPS)}
			}
			rt.Table([]string{"ID", "HOST", "PORT", "HTTPS"}, cells)
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id for ref resolution")
	cmd.Flags().StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func tileDomainRmCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <domain-id>",
		Short: "Remove a tile domain",
		Args:  domainRmArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[len(args)-1] // a leading tile ref is accepted and ignored
			if err := rt.Confirm(fmt.Sprintf("Remove domain %s?", id)); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteDomain(cmd.Context(), id); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{id, true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed.")
			return nil
		},
	}
	cmd.Flags().String("tile", "", "tile id (unused; accepted for symmetry)")
	return cmd
}

// domainRmArgs: 1 arg (the id) or 2 (legacy tile ref + id).
var domainRmArgs = cobra.RangeArgs(1, 2)

// ---- tile volume ----

func tileVolumeCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage a tile's persistent volumes",
	}
	cmd.AddCommand(tileVolumeAddCmd(rt), tileVolumeLsCmd(rt), tileVolumeRmCmd(rt))
	return cmd
}

func tileVolumeAddCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env, mount, name, volumeName string
	var maxSizeMB int
	cmd := &cobra.Command{
		Use:   "add [ref]",
		Short: "Mount a persistent volume into a tile",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mount == "" {
				return usagef("--mount <path> is required")
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := tileTarget(cmd, rt, client, args, appFlag, stack, env)
			if err != nil {
				return err
			}
			if name == "" {
				// default the name to the mount's last path segment (/data -> data)
				name = mount[strings.LastIndex(mount, "/")+1:]
				if name == "" {
					name = "data"
				}
			}
			v, err := client.AddVolumeWith(cmd.Context(), id, cli.VolumeCreate{
				Name: name, MountPath: mount, VolumeName: volumeName, MaxSizeMB: maxSizeMB})
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(v)
			}
			_, _ = fmt.Fprintf(rt.Stdout, "Added volume %s at %s (%s)\n", v.Name, v.MountPath, v.ID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&mount, "mount", "", "mount path inside the container (required)")
	f.StringVar(&name, "name", "", "volume name (default: last mount path segment)")
	f.StringVar(&volumeName, "volume-name", "", "adopt an existing docker volume instead of creating one")
	f.IntVar(&maxSizeMB, "max-size-mb", 0, "warn-only size ceiling in MB (0 = none; docker's local driver enforces no quota)")
	f.StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	f.StringVar(&stack, "stack", "", "stack id for ref resolution")
	f.StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func tileVolumeLsCmd(rt *Runtime) *cobra.Command {
	var appFlag, stack, env string
	cmd := &cobra.Command{
		Use:     "ls [ref]",
		Aliases: []string{"list"},
		Short:   "List a tile's volumes",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.Client()
			if err != nil {
				return err
			}
			id, err := tileTarget(cmd, rt, client, args, appFlag, stack, env)
			if err != nil {
				return err
			}
			vs, err := client.Volumes(cmd.Context(), id)
			if err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(vs)
			}
			cells := make([][]string, len(vs))
			for i, v := range vs {
				cells[i] = []string{v.ID, v.Name, v.MountPath, v.VolumeName}
			}
			rt.Table([]string{"ID", "NAME", "MOUNT", "VOLUME"}, cells)
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "tile", "", "tile id (alternative to the ref argument)")
	cmd.Flags().StringVar(&stack, "stack", "", "stack id for ref resolution")
	cmd.Flags().StringVar(&env, "env", "", "env id for ref resolution")
	return cmd
}

func tileVolumeRmCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <volume-id>",
		Short: "Remove a tile volume (its data is preserved)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[len(args)-1] // a leading tile ref is accepted and ignored
			if err := rt.Confirm(fmt.Sprintf("Remove volume %s? (data is preserved)", id)); err != nil {
				return err
			}
			client, err := rt.Client()
			if err != nil {
				return err
			}
			if err := client.DeleteVolume(cmd.Context(), id); err != nil {
				return err
			}
			if rt.JSON {
				return rt.EmitJSON(removed{id, true})
			}
			_, _ = fmt.Fprintln(rt.Stdout, "Removed (data preserved).")
			return nil
		},
	}
	cmd.Flags().String("tile", "", "tile id (unused; accepted for symmetry)")
	return cmd
}
