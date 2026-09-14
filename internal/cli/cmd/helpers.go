package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/FyrmForge/stackr/internal/cli"
)

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// resolveAppID resolves the consumer tile id: --tile flag, else the link.
func (rt *Runtime) resolveAppID(appFlag string) (string, error) {
	if appFlag != "" {
		return appFlag, nil
	}
	if link, _, _ := rt.Link(); link.App != "" {
		return link.App, nil
	}
	return "", fmt.Errorf("no tile; link one or pass --tile <id>")
}

// linkedEnv is the environment this directory is bound to, which several infra
// commands send so the server can resolve relative paths and place slices.
func (rt *Runtime) linkedEnv(envIDFlag string) string {
	if envIDFlag != "" {
		return envIDFlag
	}
	if l, _, err := rt.Link(); err == nil {
		return l.Env
	}
	return ""
}

// resolveApp picks the target app: an explicit ref (looked up within the
// linked stack/env, or globally if unlinked) overrides the linked default.
func resolveApp(ctx context.Context, rt *Runtime, client *cli.Client, ref string) (cli.App, error) {
	link, _, linkErr := rt.Link()
	if ref != "" {
		apps, err := client.Apps(ctx, link.Stack, link.Env)
		if err != nil {
			return cli.App{}, err
		}
		for _, a := range apps {
			if a.ID == ref || a.Name == ref {
				return a, nil
			}
		}
		where := "your tiles"
		if linkErr == nil {
			where = "the linked stack/env"
		}
		return cli.App{}, fmt.Errorf("no tile %q in %s", ref, where)
	}
	if link.App != "" {
		return cli.App{ID: link.App, Name: link.AppName}, nil
	}
	return cli.App{}, fmt.Errorf("no tile selected; run `stackr link` or pass a tile name")
}

// resolveTile is resolveApp widened to databases: both are tiles and both are
// forwardable, but the API lists them through separate endpoints. A full id
// wins over the link; a db name only matches within the linked stack.
func resolveTile(ctx context.Context, rt *Runtime, client *cli.Client, ref string) (id, name string, err error) {
	link, _, linkErr := rt.Link()
	if ref == "" {
		if link.App == "" {
			return "", "", fmt.Errorf("no tile selected; run `stackr link` or pass a tile name")
		}
		return link.App, link.AppName, nil
	}
	apps, err := client.Apps(ctx, link.Stack, link.Env)
	if err != nil {
		return "", "", err
	}
	for _, a := range apps {
		if a.ID == ref || a.Name == ref {
			return a.ID, a.Name, nil
		}
	}
	dbs, err := client.DBs(ctx)
	if err != nil {
		return "", "", err
	}
	for _, d := range dbs {
		if d.ID == ref {
			return d.ID, d.Name, nil
		}
		if d.Name == ref && (link.Stack == "" || d.StackID == link.Stack) {
			return d.ID, d.Name, nil
		}
	}
	where := "your tiles"
	if linkErr == nil {
		where = "the linked stack/env"
	}
	return "", "", fmt.Errorf("no tile %q in %s", ref, where)
}

// resolveTileID maps a tile ref (id or name) to its id via the linked
// stack/env's tile list. --stack/--env win over the link; a bare id retries
// unscoped so it works from any directory.
func resolveTileID(ctx context.Context, rt *Runtime, client *cli.Client, ref, stack, env string) (string, error) {
	if stack == "" && env == "" {
		if link, _, err := rt.Link(); err == nil {
			stack, env = link.Stack, link.Env
		}
	}
	apps, err := client.Apps(ctx, stack, env)
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		if a.ID == ref || a.Name == ref {
			return a.ID, nil
		}
	}
	if stack != "" || env != "" {
		if all, aerr := client.Apps(ctx, "", ""); aerr == nil {
			for _, a := range all {
				if a.ID == ref {
					return a.ID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no tile %q; pass --stack <id> or link a directory", ref)
}

func keepLabel(n int) string {
	if n == 0 {
		return "all"
	}
	return strconv.Itoa(n)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
