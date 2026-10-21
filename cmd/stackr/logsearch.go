package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// logLineText is a stored line as one line of text; an access line reads
// "GET /path 200 12ms".
func logLineText(m map[string]any) string {
	h := asMap(m["http"])
	if cell(m["source"]) != "access" || h == nil {
		return cell(m["msg"])
	}
	s := strings.TrimSpace(cell(h["http.method"]) + " " + cell(h["http.path"]) + " " + cell(h["http.status"]))
	if d := cell(h["http.duration_ms"]); d != "" {
		s += " " + d + "ms"
	}
	return s
}

// logsWithSearch is stackr logs (a replica's or run's live log) with its
// search verb for the kept history.
func (a *app) logsWithSearch() *cobra.Command {
	c := a.logs()
	c.AddCommand(scoped(a.logSearch(), false), scoped(a.logPurge(), false))
	return c
}

// logSearch is stackr logs search: the env's (or one --tile's) kept log lines.
func (a *app) logSearch() *cobra.Command {
	var kind, source, stream, run, since string
	c := leaf("search [text]", "env.logs", "Search the kept log lines of an environment, or of one --tile, newest first (500 at most)", upTo(1),
		a.at(atEnv, func(c *cobra.Command, p string, args []string) error {
			q := url.Values{}
			for k, v := range map[string]string{
				"tile": flag(c, "tile"), "kind": kind, "source": source, "stream": stream, "run": run, "since": since,
			} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if len(args) > 0 {
				q.Set("text", args[0])
			}
			v, err := a.call(GET, p+"/logs?"+q.Encode(), nil)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			var rows [][]string
			for _, l := range asList(v) {
				m := asMap(l)
				rows = append(rows, []string{a.stamp(cell(m["time"]), "Jan 02 15:04:05"), cell(m["tile"]), cell(m["source"]), cell(m["stream"]), logLineText(m)})
			}
			a.table([]string{"time", "tile", "source", "stream", "line"}, rows)
			if len(rows) == 500 {
				_, _ = fmt.Fprintln(a.errw, "500 lines shown; narrow it with --since or the filters")
			}
			return nil
		}))
	c.Flags().String("tile", "", "only this tile")
	c.Flags().StringVar(&kind, "kind", "", "only this tile kind (service, image, cron, function)")
	c.Flags().StringVar(&source, "source", "", "app, access, run or build")
	c.Flags().StringVar(&stream, "stream", "", "stdout or stderr")
	c.Flags().StringVar(&run, "run", "", "a cron or function run's id")
	c.Flags().StringVar(&since, "since", "1h", "how far back, up to 7d (like 15m, 6h, 24h, 7d)")
	c.Example = "  stackr logs search --env prod --tile api --source access 500"
	return c
}

// logPurge is stackr logs purge: delete the env's kept log lines.
func (a *app) logPurge() *cobra.Command {
	return leaf("purge", "env.logs.purge", "Delete every kept log line of an environment", cobra.NoArgs,
		a.at(atEnv, func(_ *cobra.Command, p string, _ []string) error {
			if err := a.confirm("Delete all kept logs of " + last(p) + "? Live logs of running tiles are not touched."); err != nil {
				return err
			}
			if _, err := a.call(DELETE, p+"/logs", nil); err != nil {
				return err
			}
			a.say("deleted the kept logs of %s", last(p))
			return nil
		}))
}
