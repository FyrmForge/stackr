package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	stackCols   = []string{"slug", "name", "description", "config_repo", "id"}
	envCols     = []string{"slug", "name", "type", "from_kind", "from_branch", "auto", "release_id", "id"}
	tileCols    = []string{"slug", "name", "kind", "image_ref", "git_url", "container_port", "id"}
	managedCols = []string{"engine", "allow", "env_pairs", "tile_id", "id"}
	sliceCols   = []string{"slug", "kind", "provision_from", "default_access", "on_remove", "id"}
	releaseCols = []string{"number", "created_by", "created_at", "id"}
	domainCols  = []string{"host", "path", "container_port", "https", "force_https", "redirect_to", "id"}
	paramCols   = []string{"collection", "name", "kind", "value", "scope_kind"}
	volumeCols  = []string{"slug", "name", "max_size_mb", "scope_kind", "orphaned_at", "id"}
	schedCols   = []string{"id", "method", "cron", "timezone", "keep", "mode", "dest_id"}
	runCols     = []string{"id", "status", "trigger", "created_at", "size_bytes", "object_key", "error"}
	tileRunCols = []string{"id", "status", "trigger", "exit_code", "reason", "created_at", "finished_at"}
)

// tileFlags are the tile settings a create or set takes: flag -> PATCH key.
// Only the flags given are sent (B1, B21, B22).
func tileFlags(c *cobra.Command) map[string]string {
	f := c.Flags()
	for _, s := range [][2]string{
		{"image", "image ref (image tiles)"},
		{"git", "git URL (service tiles)"},
		{"branch", "git branch"},
		{"dockerfile", "Dockerfile path"},
		{"context", "build context"},
		{"watch-paths", "paths whose change triggers a build, one per line"},
		{"env-json", "environment, a JSON object"},
		{"build-args", "build args, a JSON object"},
		{"volumes", "volume mounts"},
		{"command", "the command"},
		{"published-ports", "ports published on the host"},
		{"protocol", "endpoint protocol"},
		{"health-path", "HTTP health path"},
		{"healthcheck", "health check command"},
		{"user", "run as user"},
		{"devices", "devices"},
		{"restart", "restart policy"},
		{"depends-on", "tiles to start first"},
		{"files", "files to mount"},
		{"shared-net", "shared network"},
		{"update-policy", "update policy"},
		{"tag-policy", "tag policy"},
		{"schedule", "cron expression, CRON_TZ= prefix allowed (cron tiles)"},
		{"trigger", "manual or on_deploy (function tiles)"},
	} {
		f.String(s[0], "", s[1])
	}
	for _, s := range [][2]string{
		{"port", "container port"},
		{"health-interval", "health check interval, seconds"},
		{"health-timeout", "health check timeout, seconds"},
		{"health-retries", "health check retries"},
		{"health-start-period", "health check start period, seconds"},
		{"memory", "memory limit, MB"},
		{"shm-size", "shm size, MB"},
		{"replicas", "replica count"},
		{"timeout", "a run's timeout, minutes (cron and function tiles; 0 = 30)"},
	} {
		f.Int(s[0], 0, s[1])
	}
	f.Float64("cpus", 0, "CPU limit")
	f.Bool("privileged", false, "run privileged")
	return map[string]string{
		"image":               "image_ref",
		"git":                 "git_url",
		"branch":              "git_branch",
		"dockerfile":          "dockerfile_path",
		"context":             "build_context",
		"watch-paths":         "watch_paths",
		"env-json":            "env_json",
		"build-args":          "build_args",
		"volumes":             "volumes",
		"command":             "command",
		"published-ports":     "published_ports",
		"protocol":            "endpoint_protocol",
		"health-path":         "health_path",
		"healthcheck":         "healthcheck_cmd",
		"user":                "user",
		"devices":             "devices",
		"restart":             "restart_policy",
		"depends-on":          "depends_on",
		"files":               "files",
		"shared-net":          "shared_net",
		"update-policy":       "update_policy",
		"tag-policy":          "tag_policy",
		"port":                "container_port",
		"health-interval":     "healthcheck_interval_s",
		"health-timeout":      "healthcheck_timeout_s",
		"health-retries":      "healthcheck_retries",
		"health-start-period": "healthcheck_start_period_s",
		"memory":              "mem_limit_mb",
		"shm-size":            "shm_size_mb",
		"replicas":            "replicas",
		"cpus":                "cpu_limit",
		"privileged":          "privileged",
		"schedule":            "schedule",
		"trigger":             "trigger",
		"timeout":             "timeout_minutes",
	}
}

func (a *app) stackCommands() []*cobra.Command {
	return []*cobra.Command{
		a.stacks(),
		a.releases(),
		a.envs(),
		a.promote(),
		a.rollback(),
		a.tiles(),
		a.deploy(),
		a.restart(),
		a.logs(),
		a.managed(),
		a.sliceTiles(),
		a.params(),
		a.volumes(),
		a.backups(),
	}
}

// at runs f with the path down to lv. The tile is the first arg when the
// verb's Use names it there ("get [tile]", "rename <tile> <name>", a slice
// tile's "get [slice]"); a verb
// whose first arg is something else ("domain add <host>") takes the tile
// from --tile or the link.
func (a *app) at(
	lv level,
	f func(c *cobra.Command, p string, args []string) error,
) func(*cobra.Command, []string) error {
	return func(c *cobra.Command, args []string) error {
		tile := ""
		if lv == atTile && len(args) > 0 && tileFirst(c.Use) {
			tile = args[0]
		}
		p, err := a.path(c, lv, tile)
		if err != nil {
			return err
		}
		return f(c, p, args)
	}
}

func tileFirst(use string) bool {
	f := strings.Fields(use)
	return len(f) > 1 && slices.Contains([]string{"[tile]", "<tile>", "[slice]", "<slice>"}, f[1])
}

// get is a leaf that shows one GET under lv.
func (a *app) get(use, op, short string, lv level, sub string, cols ...string) *cobra.Command {
	n := 0
	if lv == atTile {
		n = 1
	}
	return leaf(use, op, short, upTo(n), a.at(lv, func(_ *cobra.Command, p string, _ []string) error {
		v, err := a.call(GET, p+sub, nil)
		if err != nil {
			return err
		}
		return a.show(v, cols...)
	}))
}

// put is a leaf that PUTs {key: arg} at p+sub.
func (a *app) put(use, op, short string, lv level, sub, key string) *cobra.Command {
	return leaf(use, op, short, exact(1), a.at(lv, func(_ *cobra.Command, p string, args []string) error {
		_, err := a.call(PUT, p+sub, map[string]string{key: args[0]})
		return err
	}))
}

// ---- stack ----

func (a *app) stacks() *cobra.Command {
	var desc string
	create := leaf("create <name>", "stack.create", "Make a stack", exact(1),
		a.at(atOrg, func(_ *cobra.Command, p string, args []string) error {
			v, err := a.call(POST, p+"/stacks", map[string]string{"name": args[0], "description": desc})
			if err != nil {
				return err
			}
			return a.show(v, stackCols...)
		}))
	create.Flags().StringVar(&desc, "desc", "", "a one-line description")
	var repo, branch, path, conn string
	configRepo := leaf("config-repo", "stack.config-repo", "Point the stack at its config repo", exact(0),
		a.at(atStack, func(_ *cobra.Command, p string, _ []string) error {
			_, err := a.call(PUT, p+"/config-repo", map[string]string{
				"repo":         repo,
				"branch":       branch,
				"path":         path,
				"connector_id": conn,
			})
			return err
		}))
	configRepo.Flags().StringVar(&repo, "repo", "", "the git URL (empty: none)")
	configRepo.Flags().StringVar(&branch, "branch", "main", "the branch")
	configRepo.Flags().StringVar(&path, "path", "stackr-compose.yml", "the stack file in the repo")
	configRepo.Flags().StringVar(&conn, "connector", "", "the connector id (stackr org connectors ls)")

	return scoped(noun("stack", "Stacks",
		leaf("ls", "stack.list", "List stacks", exact(0),
			a.at(atOrg, func(_ *cobra.Command, p string, _ []string) error {
				v, err := a.call(GET, p+"/stacks", nil)
				if err != nil {
					return err
				}
				return a.show(v, stackCols...)
			})),
		a.get("get", "stack.get", "Show the stack", atStack, "", stackCols...),
		create,
		a.put("rename <name>", "stack.rename", "Rename the stack", atStack, "/name", "name"),
		configRepo,
		a.settingsCmd("stack.get,stack.settings", "Show or change the stack's settings defaults", atStack),
		waits(leaf("image-check", "stack.image-check", "Check the stack's images for a newer tag", exact(0),
			a.at(atStack, func(c *cobra.Command, p string, _ []string) error {
				return a.orgJob(c, POST, p+"/image-check", nil, "image check")
			}))),
		leaf("rm", "stack.delete", "Delete the stack", exact(0),
			a.at(atStack, func(c *cobra.Command, p string, _ []string) error {
				if err := a.confirm("Remove stack " + last(p) + " and everything in it?"); err != nil {
					return err
				}
				_, err := a.call(DELETE, p, nil)
				return err
			})),
	), false)
}

func (a *app) releases() *cobra.Command {
	return scoped(noun("release", "Releases of the stack",
		a.get("ls", "release.list", "List releases, newest first", atStack, "/releases", releaseCols...),
		leaf("get <number>", "release.list,release.get", "Show a release and its pins", exact(1),
			a.at(atStack, func(c *cobra.Command, p string, args []string) error {
				id, err := a.release(c, args[0])
				if err != nil {
					return err
				}
				v, err := a.call(GET, p+"/releases/"+id, nil)
				if err != nil {
					return err
				}
				return a.show(v)
			})),
	), false)
}

// ---- env ----

func (a *app) envs() *cobra.Command {
	var typ, base, color, fromKind, fromBranch string
	var auto bool
	create := leaf("create <name>", "env.create", "Make an environment", exact(1),
		a.at(atStack, func(_ *cobra.Command, p string, args []string) error {
			body := map[string]any{
				"name":        args[0],
				"type":        typ,
				"color":       color,
				"from_kind":   fromKind,
				"from_branch": fromBranch,
				"auto":        auto,
			}
			if base != "" {
				body["base_env_id"] = base
			}
			v, err := a.call(POST, p+"/envs", body)
			if err != nil {
				return err
			}
			return a.show(v, envCols...)
		}))
	create.Flags().StringVar(&typ, "type", "static", "static or ephemeral")
	create.Flags().StringVar(&base, "base", "", "the env id an ephemeral env copies")
	create.Flags().StringVar(&color, "color", "", "the panel colour")
	from := leaf("from", "env.from", "Set where the env's releases come from", exact(0),
		a.at(atEnv, func(_ *cobra.Command, p string, _ []string) error {
			_, err := a.call(PUT, p+"/from", map[string]any{"kind": fromKind, "branch": fromBranch, "auto": auto})
			return err
		}))
	for _, c := range []*cobra.Command{create, from} {
		c.Flags().StringVar(&fromKind, "from-kind", "branch", "branch (builds a branch) or promote (takes releases by hand)")
		c.Flags().StringVar(&fromBranch, "from-branch", "", "the branch it builds")
		c.Flags().BoolVar(&auto, "auto", false, "deploy each new release on its own")
	}
	return scoped(noun("env", "Environments of the stack",
		a.get("ls", "env.list", "List environments", atStack, "/envs", envCols...),
		a.get("ladder", "env.ladder", "List environments in promote order", atStack, "/ladder", envCols...),
		a.get("get", "env.get", "Show the environment", atEnv, "", envCols...),
		a.get(
			"traffic",
			"env.traffic",
			"Show tile-to-tile bytes per second at the last 5 s sample",
			atEnv,
			"/traffic",
			"from",
			"to",
			"bps",
		),
		create,
		a.put("rename <name>", "env.rename", "Rename the environment", atEnv, "/name", "name"),
		a.put("color <color>", "env.color", "Set the environment's panel colour", atEnv, "/color", "color"),
		from,
		a.settingsCmd("env.get,env.settings", "Show or change the environment's settings defaults", atEnv),
		leaf("reorder <env>...", "env.list,env.reorder", "Set the promote order, bottom rung first", atLeast(1),
			a.at(atStack, func(_ *cobra.Command, p string, args []string) error {
				ids := make([]string, 0, len(args))
				for _, n := range args {
					e, err := a.find(p+"/envs", "environment", n, "slug", "name", "id")
					if err != nil {
						return err
					}
					ids = append(ids, cell(e["id"]))
				}
				_, err := a.call(PUT, p+"/envs-order", map[string]any{"ids": ids})
				return err
			})),
		leaf("rm", "env.delete", "Delete the environment", exact(0),
			a.at(atEnv, func(c *cobra.Command, p string, _ []string) error {
				if err := a.confirm("Remove environment " + last(p) + " and its tiles? Their volumes are kept until removed."); err != nil {
					return err
				}
				_, err := a.call(DELETE, p, nil)
				return err
			})),
	), false)
}

// ---- promote ----

// plan prints what a promote would change, and its blockers (B20: the
// one field the job itself refuses on).
func (a *app) plan(p, rel string) (bool, error) {
	v, err := a.call(GET, p+"/plan/"+rel, nil)
	if err != nil {
		return false, err
	}
	m, _ := v.(map[string]any)
	ok, _ := m["can_deploy"].(bool)
	if a.json {
		return ok, a.show(v)
	}
	pl, _ := m["plan"].(map[string]any)
	a.changes(pl, "warnings", "blockers")
	return ok, nil
}

// changes prints a plan's changes as a table, then each line of lists
// (a plan's warnings, notes, blockers) on stderr.
func (a *app) changes(pl map[string]any, lists ...string) {
	changes, _ := pl["changes"].([]any)
	rows := make([][]string, 0, len(changes))
	for _, c := range changes {
		ch, _ := c.(map[string]any)
		rows = append(rows, []string{
			cell(ch["kind"]),
			cell(ch["tile"]),
			cell(ch["field"]),
			cell(ch["old"]),
			cell(ch["new"]),
			cell(ch["note"]),
		})
	}
	a.table([]string{"change", "tile", "field", "old", "new", "note"}, rows)
	for _, list := range lists {
		xs, _ := pl[list].([]any)
		for _, x := range xs {
			_, _ = fmt.Fprintf(a.errw, "%s: %s\n", strings.TrimSuffix(list, "s"), cell(x))
		}
	}
}

func (a *app) promote() *cobra.Command {
	var dry bool
	c := waits(leaf("promote <release>", "release.list,promote.plan,promote.run",
		"Promote a release (number or id) into --env; the plan prints first", exact(1),
		a.at(atEnv, func(c *cobra.Command, p string, args []string) error {
			rel, err := a.release(c, args[0])
			if err != nil {
				return err
			}
			ok, err := a.plan(p, rel)
			if err != nil || dry {
				return err
			}
			if !ok {
				return errors.New("the promote is blocked; see the blockers above")
			}
			return a.orgJob(c, POST, p+"/promote/"+rel, nil, "promote")
		})))
	c.Flags().BoolVar(&dry, "dry-run", false, "print the plan and change nothing")
	return scoped(c, false)
}

func (a *app) rollback() *cobra.Command {
	var tag string
	c := waits(leaf("rollback", "release.list,promote.rollback", "Put --env back on an earlier release", exact(0),
		a.at(atEnv, func(c *cobra.Command, p string, _ []string) error {
			if tag == "" {
				return usage("--tag is required: the last good release is a judgement (stackr release ls lists them)")
			}
			rel, err := a.release(c, tag)
			if err != nil {
				return err
			}
			return a.orgJob(c, POST, p+"/rollback/"+rel, nil, "rollback")
		})))
	c.Flags().StringVar(&tag, "tag", "", "the release to go back to (number or id); required")
	return scoped(c, false)
}

// ---- tile ----

func (a *app) tileJob(use, op, short, sub, what, q string) *cobra.Command {
	return scoped(waits(leaf(use, op, short, upTo(1), a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		if q != "" {
			if err := a.confirm(fmt.Sprintf(q, last(p))); err != nil {
				return err
			}
		}
		return a.orgJob(c, POST, p+sub, nil, what)
	}))), true)
}

func (a *app) deploy() *cobra.Command {
	return a.tileJob("deploy [tile]", "tile.deploy", "Deploy the tile and follow it", "/deploy", "deploy", "")
}

func (a *app) restart() *cobra.Command {
	return a.tileJob("restart [tile]", "tile.restart", "Restart the tile's containers", "/restart", "restart", "")
}

func (a *app) logs() *cobra.Command {
	var follow bool
	var tail int
	var container, run string
	c := leaf(
		"logs [tile]",
		"tile.status,tile.logs,tile.logs-stream",
		"Print a replica's or a run's log; --follow streams it",
		upTo(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			q := fmt.Sprintf("?run=%s&tail=%d", url.QueryEscape(run), tail)
			if run == "" {
				if container == "" {
					id, err := a.firstReplica(p)
					if err != nil {
						return err
					}
					container = id
				}
				q = fmt.Sprintf("?container=%s&tail=%d", url.QueryEscape(container), tail)
			}
			if !follow {
				v, err := a.call(GET, p+"/logs"+q, nil)
				if err != nil || a.json {
					if err == nil {
						err = a.show(v)
					}
					return err
				}
				m, _ := v.(map[string]any)
				_, err = fmt.Fprint(a.out, m["log"])
				return err
			}
			res, err := a.request(GET, p+"/logs/stream"+q, nil)
			if err != nil {
				return err
			}
			defer func() { _ = res.Body.Close() }()
			err = events(res.Body, func(name string, data []byte) (bool, error) {
				if name != "line" {
					return name == "end", nil
				}
				if a.json { // NDJSON: one {"line": …} per line
					_, err := fmt.Fprintf(a.out, "{\"line\":%s}\n", data)
					return false, err
				}
				var s string
				_ = json.Unmarshal(data, &s)
				_, err := fmt.Fprintln(a.out, s)
				return false, err
			})
			if a.ctx.Err() != nil {
				return nil // Ctrl-C ends a follow; nothing to report
			}
			return err
		}),
	)
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming")
	c.Flags().IntVar(&tail, "tail", 200, "lines of history")
	c.Flags().StringVar(&container, "container", "", "the replica (default: the first)")
	c.Flags().StringVar(&run, "run", "", "a cron or function run's log instead (stackr tile runs lists them)")
	return scoped(c, true)
}

// ---- runs (cron and function tiles) ----

// runNow queues a run and follows its job; a run refused because the last
// one is still going prints the cancelled row and fails.
func (a *app) runNow() *cobra.Command {
	return scoped(waits(leaf("run [tile]", "tile.run", "Run a cron or function tile now and follow it", upTo(1),
		a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
			v, err := a.call(POST, p+"/run", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			r, _ := m["run"].(map[string]any)
			if m["job"] == nil {
				return fmt.Errorf("run %s %s: %s", cell(r["id"]), cell(r["status"]), cell(r["reason"]))
			}
			_, _ = fmt.Fprintf(a.errw, "run %s: stackr tile logs --run %s\n", cell(r["id"]), cell(r["id"]))
			if nw, _ := c.Flags().GetBool("no-wait"); nw {
				return a.show(r, tileRunCols...)
			}
			base, err := a.orgPath()
			if err != nil {
				return err
			}
			return a.follow(base, m["job"], "run")
		}))), true)
}

func (a *app) pause(use, short string, paused bool) *cobra.Command {
	return scoped(leaf(use, "tile.pause", short, upTo(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(POST, p+"/pause", map[string]bool{"paused": paused})
			if err != nil {
				return err
			}
			return a.show(v, tileCols...)
		})), true)
}

// runs lists the tile's runs; --run shows one.
func (a *app) runs() *cobra.Command {
	var run string
	c := leaf(
		"runs [tile]",
		"run.list,run.get",
		"List a cron or function tile's runs, newest first; --run shows one",
		upTo(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			sub := "/runs"
			if run != "" {
				sub += "/" + url.PathEscape(run)
			}
			v, err := a.call(GET, p+sub, nil)
			if err != nil {
				return err
			}
			return a.show(v, tileRunCols...)
		}),
	)
	c.Flags().StringVar(&run, "run", "", "the run id")
	return scoped(c, true)
}

// stop stops the tile's containers (a cron pauses instead); --run stops one run.
func (a *app) stop() *cobra.Command {
	var run string
	c := waits(leaf(
		"stop [tile]",
		"tile.stop,run.stop",
		"Stop the tile's containers; --run stops one run of a cron or function",
		upTo(1),
		a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
			if run != "" {
				_, err := a.call(DELETE, p+"/runs/"+url.PathEscape(run), nil)
				return err
			}
			return a.orgJob(c, POST, p+"/stop", nil, "stop")
		}),
	))
	c.Flags().StringVar(&run, "run", "", "the run to stop")
	return scoped(c, true)
}

// firstReplica is the tile's first container, from its status.
func (a *app) firstReplica(p string) (string, error) {
	v, err := a.call(GET, p+"/status", nil)
	if err != nil {
		return "", err
	}
	m, _ := v.(map[string]any)
	rs, _ := m["replicas"].([]any)
	if len(rs) == 0 {
		return "", errors.New("the tile has no running replica")
	}
	r, _ := rs[0].(map[string]any)
	return cell(r["id"]), nil
}

func (a *app) tiles() *cobra.Command {
	var kind string
	create := leaf("create <name>", "tile.create", "Make a tile in --env", exact(1), nil)
	createKeys := tileFlags(create)
	create.Flags().StringVar(&kind, "kind", "image", "image, service (built from git), cron or function")
	create.RunE = a.at(atEnv, func(c *cobra.Command, p string, args []string) error {
		body, err := changed(c, createKeys)
		if err != nil {
			return err
		}
		if body == nil {
			body = map[string]any{}
		}
		body["name"], body["kind"] = args[0], kind
		v, err := a.call(POST, p+"/tiles", body)
		if err != nil {
			return err
		}
		return a.show(v, tileCols...)
	})
	set := leaf("set [tile]", "tile.update", "Change tile settings; only the flags given are sent", upTo(1), nil)
	setKeys := tileFlags(set)
	set.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		body, err := changed(c, setKeys)
		if err != nil {
			return err
		}
		if body == nil {
			return usage("nothing to set; pass a flag (stackr tile set --help lists them)")
		}
		v, err := a.call(PATCH, p, body)
		if err != nil {
			return err
		}
		m, _ := v.(map[string]any)
		if j, ok := m["job"].(map[string]any); ok {
			_, _ = fmt.Fprintf(a.errw, "redeploying: stackr job log %s --follow\n", cell(j["id"]))
		}
		return a.show(m["tile"], tileCols...)
	})
	var exCont string
	exec := leaf(
		"exec [tile] -- <cmd>...",
		"tile.status,tile.exec",
		"Run a command in a replica; stdin goes to it",
		atLeast(1),
		nil,
	)
	exec.RunE = func(c *cobra.Command, args []string) error {
		tile, argv := "", args
		if n := c.ArgsLenAtDash(); n > 0 {
			tile, argv = args[0], args[n:]
		}
		if len(argv) == 0 {
			return usage("no command; stackr tile exec [tile] -- <cmd>...")
		}
		p, err := a.path(c, atTile, tile)
		if err != nil {
			return err
		}
		if exCont == "" {
			if exCont, err = a.firstReplica(p); err != nil {
				return err
			}
		}
		q := url.Values{"container": {exCont}, "cmd": argv}
		var in io.Reader = strings.NewReader("")
		if !a.tty {
			in = a.in
		}
		res, err := a.request(POST, p+"/exec?"+q.Encode(), in)
		if err != nil {
			return err
		}
		defer func() { _ = res.Body.Close() }()
		if _, err := io.Copy(a.out, res.Body); err != nil {
			return err
		}
		if code, _ := strconv.Atoi(res.Trailer.Get("X-Exit-Code")); code != 0 {
			return exitErr(code)
		}
		return nil
	}
	exec.Flags().StringVar(&exCont, "container", "", "the replica (default: the first)")

	return scoped(noun("tile", "Tiles: the deployed units",
		a.get("ls", "tile.list", "List the env's tiles", atEnv, "/tiles", tileCols...),
		a.get("get [tile]", "tile.get", "Show a tile", atTile, ""),
		create,
		set,
		leaf("rename <tile> <name>", "tile.rename", "Rename a tile", exact(2),
			a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
				_, err := a.call(PUT, p+"/name", map[string]string{"name": args[1]})
				return err
			})),
		a.tileJob(
			"rm [tile]",
			"tile.delete",
			"Remove a tile",
			"",
			"removal",
			"Remove tile %s and its containers? Its volumes are kept until removed.",
		),
		a.deploy(),
		a.restart(),
		a.stop(),
		a.tileJob("start [tile]", "tile.start", "Start the tile's containers", "/start", "start", ""),
		a.tileJob(
			"image-check [tile]",
			"tile.image-check",
			"Check the tile's image for a newer tag",
			"/image-check",
			"image check",
			"",
		),
		leaf("status [tile]", "tile.status", "Show the tile's state and replicas", upTo(1),
			a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
				v, err := a.call(GET, p+"/status", nil)
				if err != nil || a.json {
					if err == nil {
						err = a.show(v)
					}
					return err
				}
				m, _ := v.(map[string]any)
				if j, ok := m["last_job"].(map[string]any); ok {
					m["last_job"] = cell(j["kind"]) + " " + cell(j["state"]) + " " + cell(j["id"])
				}
				if r, ok := m["last_run"].(map[string]any); ok {
					m["last_run"] = cell(r["status"]) + " " + cell(r["trigger"]) + " " + cell(r["id"])
				}
				rs, _ := m["replicas"].([]any)
				ids := make([]string, 0, len(rs))
				for _, r := range rs {
					c, _ := r.(map[string]any)
					ids = append(ids, cell(c["id"]))
				}
				m["replicas"] = strings.Join(ids, " ")
				return a.show(m, "word", "replicas", "last_job", "last_run", "next_run", "paused")
			})),
		a.logs(),
		exec,
		a.runNow(),
		a.pause("pause [tile]", "Pause a cron tile's schedule", true),
		a.pause("resume [tile]", "Resume a cron tile's schedule", false),
		a.runs(),
		a.get("jobs [tile]", "tile.jobs", "List the tile's recent jobs", atTile, "/jobs", jobCols...),
		a.domains(),
		a.allow(),
		a.envPairs(),
		a.sliceAccess(),
	), true)
}

func (a *app) domains() *cobra.Command {
	domainFlags := func(c *cobra.Command) map[string]string {
		c.Flags().String("path", "", "a path prefix")
		c.Flags().Int("port", 0, "the container port (0: the tile's)")
		c.Flags().Bool("https", true, "serve HTTPS")
		c.Flags().Bool("force-https", true, "redirect HTTP to HTTPS")
		c.Flags().String("redirect-to", "", "redirect every request here")
		return map[string]string{
			"path":        "path",
			"port":        "port",
			"https":       "https",
			"force-https": "force_https",
			"redirect-to": "redirect_to",
		}
	}
	add := leaf("add <host>", "domain.attach", "Route a host to the tile", exact(1), nil)
	addKeys := domainFlags(add)
	add.RunE = a.at(atTile, func(c *cobra.Command, p string, args []string) error {
		body, err := changed(c, addKeys)
		if err != nil {
			return err
		}
		if body == nil {
			body = map[string]any{}
		}
		body["host"] = args[0]
		v, err := a.call(POST, p+"/domains", body)
		if err != nil {
			return err
		}
		return a.show(v, domainCols...)
	})
	set := leaf(
		"set <host>",
		"domain.list,domain.update",
		"Change a domain; only the flags given change",
		exact(1),
		nil,
	)
	setKeys := domainFlags(set)
	set.RunE = a.at(atTile, func(c *cobra.Command, p string, args []string) error {
		ch, err := changed(c, setKeys)
		if err != nil || ch == nil {
			if err == nil {
				err = usage("nothing to set; pass a flag (stackr tile domain set --help)")
			}
			return err
		}
		cur, err := a.find(p+"/domains", "domain", args[0], "host", "id")
		if err != nil {
			return err
		}
		body := map[string]any{
			"host":        cur["host"],
			"path":        cur["path"],
			"port":        cur["container_port"],
			"https":       cur["https"],
			"force_https": cur["force_https"],
			"redirect_to": cur["redirect_to"],
		}
		if pj, _ := cur["proxy_json"].(string); pj != "" {
			var x any
			if json.Unmarshal([]byte(pj), &x) == nil {
				body["proxy"] = x
			}
		}
		for k, v := range ch {
			body[k] = v
		}
		v, err := a.call(PUT, p+"/domains/"+cell(cur["id"]), body)
		if err != nil {
			return err
		}
		return a.show(v, domainCols...)
	})
	var file string
	caddy := leaf(
		"caddy <host>",
		"domain.list,domain.raw-caddy",
		"Set a domain's raw Caddy config (admin); -f - reads stdin, empty clears",
		exact(1),
		a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
			raw, err := readFile(a, file)
			if err != nil {
				return err
			}
			cur, err := a.find(p+"/domains", "domain", args[0], "host", "id")
			if err != nil {
				return err
			}
			_, err = a.call(
				PUT,
				p+"/domains/"+cell(cur["id"])+"/raw-caddy",
				map[string]string{"raw_caddy": string(raw)},
			)
			return err
		}),
	)
	caddy.Flags().StringVarP(&file, "file", "f", "", "the Caddy JSON (\"-\" for stdin; empty clears)")
	return noun("domain", "The tile's domains",
		a.get("ls [tile]", "domain.list", "List the tile's domains", atTile, "/domains", domainCols...),
		add,
		set,
		caddy,
		leaf("rm <host>", "domain.list,domain.detach", "Stop routing a host to the tile", exact(1),
			a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
				cur, err := a.find(p+"/domains", "domain", args[0], "host", "id")
				if err != nil {
					return err
				}
				if err := a.confirm("Remove domain " + args[0] + "? It stops routing at once."); err != nil {
					return err
				}
				_, err = a.call(DELETE, p+"/domains/"+cell(cur["id"]), nil)
				return err
			})),
	)
}

// allow edits a managed tile's allow list: --add and --rm read the list
// and send it whole, --set replaces it.
func (a *app) allow() *cobra.Command {
	var add, rm, set []string
	c := leaf(
		"allow [tile]",
		"managed.allow,tile.get,managed.list",
		"Edit who outside the env may cut slices from a managed tile (org:stack:env:tile patterns)",
		upTo(1),
		nil,
	)
	c.Flags().StringArrayVar(&add, "add", nil, "add a pattern, e.g. acme:shop:*")
	c.Flags().StringArrayVar(&rm, "rm", nil, "remove a pattern")
	c.Flags().StringArrayVar(&set, "set", nil, "replace the list (--set '' empties it)")
	c.RunE = a.at(atTile, func(c *cobra.Command, p string, _ []string) error {
		replace := c.Flags().Changed("set")
		if len(add) == 0 && len(rm) == 0 && !replace {
			return usage("give --add, --rm or --set")
		}
		list := []string{}
		if replace {
			for _, s := range set {
				if s != "" {
					list = append(list, s)
				}
			}
		} else {
			cur, err := a.allowOf(c, p)
			if err != nil {
				return err
			}
			list = cur
		}
		for _, s := range add {
			if !slices.Contains(list, s) {
				list = append(list, s)
			}
		}
		for _, s := range rm {
			i := slices.Index(list, s)
			if i < 0 {
				return usage("%s is not in the allow list", s)
			}
			list = slices.Delete(list, i, i+1)
		}
		v, err := a.call(PUT, p+"/allow", map[string]any{"allow": list})
		if err != nil {
			return err
		}
		return a.show(v, managedCols...)
	})
	return c
}

// allowOf is the allow list of the managed tile at p, read from its env's
// managed list.
func (a *app) allowOf(c *cobra.Command, p string) ([]string, error) {
	v, err := a.call(GET, p, nil)
	if err != nil {
		return nil, err
	}
	t, _ := v.(map[string]any)
	ep, err := a.path(c, atEnv, "")
	if err != nil {
		return nil, err
	}
	m, err := a.find(ep+"/managed", "managed tile", cell(t["id"]), "tile_id")
	if err != nil {
		return nil, err
	}
	xs, _ := m["allow"].([]any)
	out := []string{}
	for _, x := range xs {
		out = append(out, cell(x))
	}
	return out, nil
}

// envPairs replaces a managed tile's env pairs.
func (a *app) envPairs() *cobra.Command {
	var empty bool
	c := leaf(
		"env-pairs <tile> [consumer-env=env ...]",
		"managed.env-pairs",
		"Replace a managed tile's env pairs: a consumer's env name to one of this stack's envs",
		atLeast(1),
		nil,
	)
	c.Flags().BoolVar(&empty, "clear", false, "empty the map")
	c.RunE = a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
		pairs := map[string]string{}
		for _, s := range args[1:] {
			k, v, err := kv(s)
			if err != nil {
				return err
			}
			pairs[k] = v
		}
		switch {
		case empty && len(pairs) > 0:
			return usage("--clear takes no pairs")
		case !empty && len(pairs) == 0:
			return usage("give consumer-env=env pairs, or --clear")
		}
		v, err := a.call(PUT, p+"/env-pairs", map[string]any{"env_pairs": pairs})
		if err != nil {
			return err
		}
		return a.show(v, managedCols...)
	})
	return c
}

// sliceAccess sets a consumer's access to one slice tile of its env.
func (a *app) sliceAccess() *cobra.Command {
	var sl, access string
	c := leaf(
		"access [tile]",
		"slice.access",
		"Set the tile's access to a slice tile of its env; a bound tile is re-granted in place",
		upTo(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(PUT, p+"/slice-access", map[string]string{
				"slice":  sl,
				"access": access,
			})
			if err != nil {
				return err
			}
			return a.show(v, "slug", "slice_access")
		}),
	)
	c.Flags().StringVar(&sl, "slice", "", "the slice tile's slug")
	c.Flags().StringVar(&access, "access", "", "read or write")
	_ = c.MarkFlagRequired("slice")
	_ = c.MarkFlagRequired("access")
	return c
}

// sliceTiles is the slice noun: a database or bucket cut from a managed
// tile, bound by the tiles of its env that ref it or name it in slice_access.
func (a *app) sliceTiles() *cobra.Command {
	var from, access string
	add := leaf(
		"add <slug>",
		"slice.create",
		"Make a slice tile in --env, cut from --from at its deploy",
		exact(1),
		a.at(atEnv, func(_ *cobra.Command, p string, args []string) error {
			v, err := a.call(POST, p+"/slices", map[string]string{
				"slug":           args[0],
				"provision_from": from,
				"default_access": access,
			})
			if err != nil {
				return err
			}
			return a.show(v, sliceCols...)
		}),
	)
	add.Flags().StringVar(&from, "from", "", "the managed tile, <stack>:<env>:<tile>")
	add.Flags().StringVar(&access, "access", "", "the default access of its consumers, read or write (default write)")
	_ = add.MarkFlagRequired("from")
	return scoped(noun("slice", "Slice tiles: a database or bucket cut from a managed tile",
		add,
		a.get("get [slice]", "slice.get", "Show a slice tile and the instance it resolves to", atTile, "/slice"),
		a.get(
			"bindings [slice]",
			"slice.bindings",
			"List the tiles holding a cred on the slice",
			atTile,
			"/bindings",
			"consumer",
			"kind",
			"access",
			"user",
			"since",
		),
		leaf(
			"on-remove <slice> <keep|drop>",
			"slice.on-remove",
			"Keep or drop the data when the slice tile is removed",
			exact(2),
			a.at(atTile, func(_ *cobra.Command, p string, args []string) error {
				v, err := a.call(PUT, p+"/on-remove", map[string]string{"on_remove": args[1]})
				if err != nil {
					return err
				}
				return a.show(v, sliceCols...)
			}),
		),
	), true)
}

func (a *app) managed() *cobra.Command {
	var engine string
	create := leaf("create <name>", "managed.create", "Make a managed tile (postgres, s3) in --env", exact(1), nil)
	keys := tileFlags(create)
	create.Flags().StringVar(&engine, "engine", "", "postgres or s3")
	_ = create.MarkFlagRequired("engine")
	create.RunE = a.at(atEnv, func(c *cobra.Command, p string, args []string) error {
		body, err := changed(c, keys)
		if err != nil {
			return err
		}
		if body == nil {
			body = map[string]any{}
		}
		body["name"], body["engine"] = args[0], engine
		v, err := a.call(POST, p+"/managed", body)
		if err != nil {
			return err
		}
		return a.show(v)
	})
	return scoped(noun("managed", "Managed tiles",
		a.get("ls", "managed.list", "List the env's managed tiles", atEnv, "/managed", managedCols...),
		create,
	), true)
}

// ---- params ----

// param splits "collection.NAME".
func param(s string) (string, string, error) {
	c, n, ok := strings.Cut(s, ".")
	if !ok || c == "" || n == "" {
		return "", "", usage("%q is not collection.NAME", s)
	}
	return c, n, nil
}

func (a *app) params() *cobra.Command {
	var reveal, secret, force bool
	var out, file string
	get := leaf("get [collection.NAME]", "org.params,stack.params,env.params,org.secrets,stack.secrets,env.secrets",
		"List params at the level; secrets stay masked unless --reveal", upTo(1), nil)
	get.RunE = func(c *cobra.Command, args []string) error {
		p, err := a.levelPath(c)
		if err != nil {
			return err
		}
		sub := "/params"
		if reveal {
			sub += "/secrets"
		}
		v, err := a.call(GET, p+sub, nil)
		if err != nil {
			return err
		}
		if len(args) == 1 {
			col, name, err := param(args[0])
			if err != nil {
				return err
			}
			xs, _ := v.([]any)
			for _, x := range xs {
				m, _ := x.(map[string]any)
				if m["collection"] == col && m["name"] == name {
					return a.show(m, paramCols...)
				}
			}
			return fmt.Errorf("no param %s at this level", args[0])
		}
		return a.show(v, paramCols...)
	}
	get.Flags().BoolVar(&reveal, "reveal", false, "show secret values (needs write access; audited)")
	send := func(c *cobra.Command, lines []string) error {
		p, err := a.levelPath(c)
		if err != nil {
			return err
		}
		kind := "param"
		if secret {
			kind = "secret"
		}
		body := make([]map[string]string, 0, len(lines))
		for _, l := range lines {
			k, v, err := kv(l)
			if err != nil {
				return err
			}
			col, name, err := param(k)
			if err != nil {
				return err
			}
			body = append(body, map[string]string{
				"collection": col,
				"name":       name,
				"kind":       kind,
				"value":      v,
			})
		}
		if len(body) == 0 {
			return usage("nothing to set")
		}
		_, err = a.call(PATCH, p+"/params", body)
		return err
	}
	set := leaf("set <collection.NAME=value>...", "org.params-set,stack.params-set,env.params-set",
		"Set params; others at the level stay", atLeast(1),
		func(c *cobra.Command, args []string) error { return send(c, args) })
	merge := leaf("merge", "org.params-set,stack.params-set,env.params-set",
		"Set every collection.NAME=value line of a file (-f, \"-\" stdin); others stay", exact(0),
		func(c *cobra.Command, _ []string) error {
			b, err := readFile(a, file)
			if err != nil {
				return err
			}
			var lines []string
			sc := bufio.NewScanner(strings.NewReader(string(b)))
			for sc.Scan() {
				if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
					lines = append(lines, l)
				}
			}
			return send(c, lines)
		})
	merge.Flags().StringVarP(&file, "file", "f", "-", "the file (\"-\" for stdin)")
	for _, c := range []*cobra.Command{set, merge} {
		c.Flags().BoolVar(&secret, "secret", false, "store the values as secrets")
	}
	export := leaf("export", "org.secrets,stack.secrets,env.secrets",
		"Write every param, secrets included, as collection.NAME=value; the one command that puts secrets on disk", exact(0),
		func(c *cobra.Command, _ []string) error {
			p, err := a.levelPath(c)
			if err != nil {
				return err
			}
			v, err := a.call(GET, p+"/params/secrets", nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			var sb strings.Builder
			xs, _ := v.([]any)
			for _, x := range xs {
				m, _ := x.(map[string]any)
				fmt.Fprintf(&sb, "%s.%s=%s\n", m["collection"], m["name"], m["value"])
			}
			if out == "" || out == "-" {
				_, err = io.WriteString(a.out, sb.String())
				return err
			}
			mode := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if force {
				mode = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			f, err := os.OpenFile(out, mode, 0o600)
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s exists; --force overwrites it", out)
			}
			if err != nil {
				return err
			}
			if _, err := f.WriteString(sb.String()); err != nil {
				_ = f.Close()
				return err
			}
			return f.Close()
		})
	export.Flags().StringVarP(&out, "output", "o", "", "the file (0600; default stdout)")
	export.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	return levelFlag(noun("params", "The param store: org, stack and env levels",
		get,
		set,
		merge,
		export,
		leaf(
			"rm <collection.NAME>",
			"org.param-delete,stack.param-delete,env.param-delete",
			"Delete a param at the level",
			exact(1),
			func(c *cobra.Command, args []string) error {
				p, err := a.levelPath(c)
				if err != nil {
					return err
				}
				col, name, err := param(args[0])
				if err != nil {
					return err
				}
				if err := a.confirm("Delete param " + args[0] + "? Tiles that read it fail their next deploy until it is set again."); err != nil {
					return err
				}
				_, err = a.call(DELETE, p+"/params/"+url.PathEscape(col)+"/"+url.PathEscape(name), nil)
				return err
			},
		),
	))
}

func readFile(a *app, name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(a.in)
	}
	if name == "" {
		return nil, nil
	}
	return os.ReadFile(name)
}

// ---- volumes and backups ----

func (a *app) volumes() *cobra.Command {
	var maxMB int
	add := leaf(
		"add <slug>",
		"org.volume-declare,stack.volume-declare,env.volume-declare",
		"Declare a volume at the level",
		exact(1),
		func(c *cobra.Command, args []string) error {
			p, err := a.levelPath(c)
			if err != nil {
				return err
			}
			v, err := a.call(POST, p+"/volumes", map[string]any{"slug": args[0], "max_size_mb": maxMB})
			if err != nil {
				return err
			}
			return a.show(v, volumeCols...)
		},
	)
	add.Flags().IntVar(&maxMB, "max-mb", 0, "size cap in MB (0: none)")
	return levelFlag(noun("volume", "Volumes",
		leaf("ls", "org.volumes,stack.volumes,env.volumes", "List volumes at the level", exact(0),
			func(c *cobra.Command, _ []string) error {
				p, err := a.levelPath(c)
				if err != nil {
					return err
				}
				v, err := a.call(GET, p+"/volumes", nil)
				if err != nil {
					return err
				}
				return a.show(v, volumeCols...)
			}),
		add,
		leaf("rm <volume-id>", "volume.delete", "Delete a volume and its data", exact(1),
			a.at(atOrg, func(_ *cobra.Command, p string, args []string) error {
				if err := a.confirm("Delete volume " + args[0] + " and the data in it? Backups already taken are kept."); err != nil {
					return err
				}
				_, err := a.call(DELETE, p+"/volumes/"+args[0], nil)
				return err
			})),
	))
}

func (a *app) backups() *cobra.Command {
	org := func(f func(c *cobra.Command, p string, args []string) error) func(*cobra.Command, []string) error {
		return a.at(atOrg, f)
	}
	list := func(use, op, short, sub string, cols ...string) *cobra.Command {
		return leaf(use, op, short, exact(1), org(func(_ *cobra.Command, p string, args []string) error {
			v, err := a.call(GET, p+"/volumes/"+args[0]+sub, nil)
			if err != nil {
				return err
			}
			return a.show(v, cols...)
		}))
	}
	schedKeys := func(c *cobra.Command) map[string]string {
		c.Flags().String("method", "", "the backup method (stackr backup methods lists them)")
		c.Flags().String("dest", "", "the destination id (default: local)")
		c.Flags().String("schedule", "", "a cron expression")
		c.Flags().String("tz", "UTC", "the cron's timezone")
		c.Flags().Int("keep", 7, "archives to keep (0: all)")
		c.Flags().String("mode", "", "the method's mode")
		return map[string]string{
			"method":   "method",
			"dest":     "dest_id",
			"schedule": "cron",
			"tz":       "timezone",
			"keep":     "keep",
			"mode":     "mode",
		}
	}
	create := leaf("create <volume-id>", "schedule.create", "Schedule backups of a volume", exact(1), nil)
	createKeys := schedKeys(create)
	create.RunE = org(func(c *cobra.Command, p string, args []string) error {
		body := map[string]any{
			"method":   flag(c, "method"),
			"cron":     flag(c, "schedule"),
			"timezone": flag(c, "tz"),
			"mode":     flag(c, "mode"),
		}
		body["keep"], _ = c.Flags().GetInt("keep")
		ch, err := changed(c, createKeys)
		if err != nil {
			return err
		}
		for k, v := range ch {
			body[k] = v
		}
		v, err := a.call(POST, p+"/volumes/"+args[0]+"/schedules", body)
		if err != nil {
			return err
		}
		return a.show(v, schedCols...)
	})
	set := leaf(
		"set <volume-id> <schedule-id>",
		"schedule.list,schedule.update",
		"Change a backup schedule; only the flags given change",
		exact(2),
		nil,
	)
	setKeys := schedKeys(set)
	set.RunE = org(func(c *cobra.Command, p string, args []string) error {
		ch, err := changed(c, setKeys)
		if err != nil || ch == nil {
			if err == nil {
				err = usage("nothing to set; pass a flag (stackr backup set --help)")
			}
			return err
		}
		cur, err := a.find(p+"/volumes/"+args[0]+"/schedules", "schedule", args[1], "id")
		if err != nil {
			return err
		}
		body := map[string]any{
			"method":   cur["method"],
			"dest_id":  cur["dest_id"],
			"cron":     cur["cron"],
			"timezone": cur["timezone"],
			"keep":     cur["keep"],
			"mode":     cur["mode"],
		}
		for k, v := range ch {
			body[k] = v
		}
		v, err := a.call(PUT, p+"/schedules/"+args[1], body)
		if err != nil {
			return err
		}
		return a.show(v, schedCols...)
	})
	var dest, method, mode, runID, target string
	now := waits(leaf("run <volume-id>", "backup.now", "Back a volume up now", exact(1),
		org(func(c *cobra.Command, p string, args []string) error {
			return a.orgJob(
				c,
				POST,
				p+"/volumes/"+args[0]+"/backups",
				map[string]string{"dest_id": dest, "method": method, "mode": mode},
				"backup",
			)
		})))
	now.Flags().StringVar(&dest, "dest", "", "the destination id (default: local)")
	now.Flags().StringVar(&method, "method", "", "the backup method")
	now.Flags().StringVar(&mode, "mode", "", "the method's mode")
	restore := waits(leaf("restore <volume-id>", "backup.restore", "Restore a backup run over a volume", exact(1),
		org(func(c *cobra.Command, p string, args []string) error {
			if runID == "" {
				return usage("--run is required; stackr backup runs <volume-id> lists them")
			}
			if target == "" {
				target = args[0]
			}
			if err := a.confirm("Restore run " + runID + " over volume " + target + "'s live data? This cannot be undone."); err != nil {
				return err
			}
			return a.orgJob(
				c,
				POST,
				p+"/volumes/"+args[0]+"/restore",
				map[string]string{"run_id": runID, "target_volume_id": target},
				"restore",
			)
		})))
	restore.Flags().StringVar(&runID, "run", "", "the backup run to restore (required)")
	restore.Flags().StringVar(&target, "target", "", "restore into this volume instead (default: the source)")
	return noun("backup", "Volume backups",
		list("methods <volume-id>", "backup.methods", "List the ways a volume can be backed up", "/methods"),
		list("ls <volume-id>", "schedule.list", "List a volume's backup schedules", "/schedules", schedCols...),
		create,
		set,
		leaf("rm <schedule-id>", "schedule.delete", "Delete a backup schedule", exact(1),
			org(func(_ *cobra.Command, p string, args []string) error {
				if err := a.confirm("Remove backup schedule " + args[0] + "? Archives already in the bucket are kept."); err != nil {
					return err
				}
				_, err := a.call(DELETE, p+"/schedules/"+args[0], nil)
				return err
			})),
		list("runs <volume-id>", "backup.list", "List a volume's backup runs", "/runs", runCols...),
		now,
		restore,
	)
}

// last is the final segment of a path: the name a confirmation shows.
func last(p string) string {
	s, _ := url.PathUnescape(p[strings.LastIndex(p, "/")+1:])
	return s
}
