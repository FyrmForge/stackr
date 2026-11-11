package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// dbStatus is `stackr db status`: a postgres instance's health, the same
// read as the panel's Database tab, mirroring the API.
func (a *app) dbStatus() *cobra.Command {
	var top bool
	c := leaf("status", "operated.health", "Show a postgres instance's health: connections, oldest transaction, standby lag, pooler", exact(0),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/health", nil)
			if err != nil {
				return err
			}
			m, _ := v.(map[string]any)
			if a.json || m == nil {
				return a.show(v)
			}
			a.printHealth(m, top)
			return nil
		}))
	c.Flags().BoolVar(&top, "top", false, "also list the costliest statements")
	return c
}

func (a *app) printHealth(m map[string]any, top bool) {
	if m["sampled"] != true {
		a.say("not sampled yet")
		return
	}
	a.say("sampled at     %s", cell(m["at"]))
	a.say("connections    %.0f of %.0f", num(m["conn_used"]), num(m["conn_max"]))
	a.say("oldest xact    %s", (time.Duration(num(m["oldest_xact_s"])) * time.Second).String())
	a.say("archiving      %s", map[bool]string{true: "on", false: "off"}[m["archiving"] == true])
	if po, ok := m["pooler"].(map[string]any); ok {
		if po["counted"] == true {
			a.say("pooler         %s, %.0f clients active, %.0f waiting", cell(po["status"]), num(po["active"]), num(po["waiting"]))
		} else {
			a.say("pooler         %s", cell(po["status"]))
		}
	}
	if sb, _ := m["standbys"].([]any); len(sb) > 0 {
		a.say("")
		var rows [][]string
		for _, r := range sb {
			r, _ := r.(map[string]any)
			rows = append(rows, []string{cell(r["name"]), fmt.Sprintf("%.0f s", num(r["lag_seconds"])), metricText(num(r["lag_bytes"]), "bytes")})
		}
		a.table([]string{"standby", "lag", "behind"}, rows)
	}
	if sl, _ := m["slot_wal"].([]any); len(sl) > 0 {
		a.say("")
		var rows [][]string
		for _, r := range sl {
			r, _ := r.(map[string]any)
			rows = append(rows, []string{cell(r["name"]), metricText(num(r["bytes"]), "bytes")})
		}
		a.table([]string{"slot", "wal held"}, rows)
	}
	if !top {
		return
	}
	a.say("")
	qs, _ := m["top"].([]any)
	if len(qs) == 0 {
		if e, _ := m["top_error"].(string); e != "" {
			a.say("%s", e)
		} else {
			a.say("no statements")
		}
		return
	}
	var rows [][]string
	for _, q := range qs {
		q, _ := q.(map[string]any)
		rows = append(rows, []string{fmt.Sprintf("%.0f", num(q["calls"])), fmt.Sprintf("%.0f ms", num(q["total_ms"])), fmt.Sprintf("%.0f us", num(q["mean_us"])), cell(q["text"])})
	}
	a.table([]string{"calls", "total", "mean", "statement"}, rows)
}
