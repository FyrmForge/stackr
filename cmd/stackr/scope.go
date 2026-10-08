package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type level int

const (
	atOrg level = iota
	atStack
	atEnv
	atTile
)

// orgLevel are the verbs under a scoped noun that act on the org, so
// --stack/--env mean nothing to them.
var orgLevel = map[string]bool{"stack.list": true, "stack.create": true, "volume.delete": true}

// scoped gives a noun's verbs the --stack/--env (and --tile) flags that beat
// the directory link; org-level verbs do not get them.
func scoped(c *cobra.Command, tile bool) *cobra.Command {
	if len(c.Commands()) == 0 {
		if orgLevel[c.Annotations["op"]] {
			return c
		}
		add := func(name, usage string) {
			if c.Flags().Lookup(name) == nil {
				c.Flags().String(name, "", usage)
			}
		}
		add("stack", "the stack (default: the linked one)")
		add("env", "the environment (default: the linked one)")
		if tile {
			add("tile", "the tile (default: the linked one)")
		}
		return c
	}
	for _, sub := range c.Commands() {
		scoped(sub, tile)
	}
	return c
}

func flag(c *cobra.Command, name string) string {
	if f := c.Flags().Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

func (a *app) orgPath() (string, error) {
	slug := a.env("ORG", a.cfg.Org)
	if slug == "" {
		return "", errors.New("no org; run stackr login <url> --org <slug>, or set STACKR_ORG")
	}
	return "/orgs/" + url.PathEscape(slug), nil
}

// path is the API path down to lv: a flag beats the link, and a level
// that is neither given nor linked is a usage error naming the flag.
// tile, when set, is the positional tile name.
func (a *app) path(c *cobra.Command, lv level, tile string) (string, error) {
	p, err := a.orgPath()
	if err != nil || lv == atOrg {
		return p, err
	}
	_, l := a.here()
	for _, s := range []struct {
		lv           level
		name, linked string
		seg          string
	}{
		{atStack, "stack", l.Stack, "/stacks/"},
		{atEnv, "env", l.Env, "/envs/"},
		{atTile, "tile", l.Tile, "/tiles/"},
	} {
		if s.lv > lv {
			break
		}
		v := flag(c, s.name)
		if s.lv == atTile && tile != "" {
			v = tile
		}
		if v == "" {
			v = s.linked
		}
		if v == "" {
			return "", usage("no %s; pass --%s <name> or run stackr link", s.name, s.name)
		}
		p += s.seg + url.PathEscape(v)
	}
	return p, nil
}

// levelPath is the params/volumes scope: --level names it, and a named
// level that does not resolve is an error, never a quieter level.
// Unnamed, the most specific one given or linked.
func (a *app) levelPath(c *cobra.Command) (string, error) {
	switch lv := flag(c, "level"); lv {
	case "org":
		return a.path(c, atOrg, "")
	case "stack":
		return a.path(c, atStack, "")
	case "env":
		return a.path(c, atEnv, "")
	case "server":
		return "/admin", nil // admin only; read by the server file and nothing else
	case "":
	default:
		return "", usage("--level is org, stack, env or server, not %q", lv)
	}
	for _, lv := range []level{atEnv, atStack} {
		if p, err := a.path(c, lv, ""); err == nil {
			return p, nil
		}
	}
	return a.path(c, atOrg, "")
}

func levelFlag(c *cobra.Command) *cobra.Command {
	c.PersistentFlags().String("level", "", "org, stack, env or server (default: the most specific given or linked)")
	return scoped(c, false)
}

// waits gives a job verb --no-wait.
func waits(c *cobra.Command) *cobra.Command {
	c.Flags().Bool("no-wait", false, "print the job and return; it runs server-side")
	return c
}

// job sends a verb that answers with a job and follows it (base is where
// the job is read: the org, or /admin).
func (a *app) job(c *cobra.Command, base, method, path string, body any, what string) error {
	j, err := a.call(method, path, body)
	if err != nil {
		return err
	}
	if nw, _ := c.Flags().GetBool("no-wait"); nw {
		return a.show(j, "id", "kind", "state")
	}
	return a.follow(base, j, what)
}

// orgJob is job for a verb whose job lives in the caller's org.
func (a *app) orgJob(c *cobra.Command, method, path string, body any, what string) error {
	base, err := a.orgPath()
	if err != nil {
		return err
	}
	return a.job(c, base, method, path, body, what)
}

// find is the row of a list whose field matches want.
func (a *app) find(path, what, want string, fields ...string) (map[string]any, error) {
	v, err := a.call(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	xs, _ := v.([]any)
	for _, x := range xs {
		m, _ := x.(map[string]any)
		for _, f := range fields {
			if cell(m[f]) == want {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("no %s %q", what, want)
}

// release resolves a release number or id in the stack.
func (a *app) release(c *cobra.Command, ref string) (string, error) {
	if ref == "" {
		return "", usage("a release is required (a number or id from stackr release ls); stackr does not guess the latest")
	}
	sp, err := a.path(c, atStack, "")
	if err != nil {
		return "", err
	}
	r, err := a.find(sp+"/releases", "release", strings.TrimPrefix(ref, "#"), "number", "id")
	if err != nil {
		return "", err
	}
	return r["id"].(string), nil
}

// kv splits "a.b=c" style arguments.
func kv(s string) (string, string, error) {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return "", "", usage("%q is not KEY=VALUE", s)
	}
	return k, v, nil
}
