package main

import (
	"fmt"
	"maps"
	"net/url"
	"slices"

	"github.com/spf13/cobra"
)

// metricMeasures are the measures a metrics table shows, in order, and how
// each reads.
var metricMeasures = []struct{ name, unit string }{
	{"cpu_pct", "pct"},
	{"mem", "bytes"},
	{"mem_used", "bytes"},
	{"mem_total", "bytes"},
	{"disk_used", "bytes"},
	{"disk_total", "bytes"},
	{"net_rx_bps", "bps"},
	{"net_tx_bps", "bps"},
	{"io_read_bps", "bps"},
	{"io_write_bps", "bps"},
	{"rtt_ms", "ms"},
	{"loss", "ratio"},
	{"rx_bps", "bps"},
	{"tx_bps", "bps"},
	{"handshake_age_s", "s"},
	{"req_s", "rps"},
	{"4xx_s", "rps"},
	{"5xx_s", "rps"},
	{"latency_ms", "ms"},
}

var metricCols = []string{"series", "measure", "latest", "min", "avg", "max"}

// metricRows is one row per measure of a series ({measure: [{t, v}]}) with
// points: its latest, min, avg and max over the range.
func metricRows(who string, series any) [][]string {
	s := asMap(series)
	var rows [][]string
	for _, m := range metricMeasures {
		pts := asList(s[m.name])
		if len(pts) == 0 {
			continue
		}
		vs := make([]float64, len(pts))
		sum := 0.0
		for i, p := range pts {
			vs[i] = num(asMap(p)["v"])
			sum += vs[i]
		}
		rows = append(rows, []string{who, m.name,
			metricText(vs[len(vs)-1], m.unit), metricText(slices.Min(vs), m.unit),
			metricText(sum/float64(len(vs)), m.unit), metricText(slices.Max(vs), m.unit)})
	}
	return rows
}

func metricText(v float64, unit string) string {
	switch unit {
	case "pct":
		return fmt.Sprintf("%.1f%%", v)
	case "ratio":
		return fmt.Sprintf("%.1f%%", v*100)
	case "ms":
		return fmt.Sprintf("%.1f ms", v)
	case "rps":
		return fmt.Sprintf("%.2f/s", v)
	case "s":
		return fmt.Sprintf("%.0f s", v)
	}
	suffix := map[string]string{"bps": "/s"}[unit]
	for _, u := range []struct {
		n float64
		s string
	}{{1 << 30, "GB"}, {1 << 20, "MB"}, {1 << 10, "KB"}} {
		if v >= u.n {
			return fmt.Sprintf("%.1f %s%s", v/u.n, u.s, suffix)
		}
	}
	return fmt.Sprintf("%.0f B%s", v, suffix)
}

func rangeFlag(c *cobra.Command, r *string) {
	c.Flags().StringVar(r, "range", "1h", "how far back, up to 24h, or the history retention when on (like 15m, 6h, 168h)")
}

// nodeMetrics is a node's host series as a table (server admin).
func (a *app) nodeMetrics() *cobra.Command {
	var rng string
	c := leaf("metrics <name>", "admin.node-metrics", "Show a node's CPU, memory and disk over --range (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			v, err := a.call(GET, "/admin/nodes/"+url.PathEscape(args[0])+"/metrics?range="+url.QueryEscape(rng), nil)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			a.table(metricCols, metricRows(args[0], v))
			return nil
		})
	rangeFlag(c, &rng)
	c.Example = "  stackr node metrics ct110 --range 6h"
	return c
}

// tileMetrics is a tile's series, the total then each replica, as a table.
func (a *app) tileMetrics() *cobra.Command {
	var rng string
	c := leaf("metrics [tile]", "tile.metrics", "Show a tile's CPU, memory, network, disk IO and requests over --range", upTo(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			v, err := a.call(GET, p+"/metrics?range="+url.QueryEscape(rng), nil)
			if err != nil || a.json {
				if err == nil {
					err = a.show(v)
				}
				return err
			}
			m := asMap(v)
			rows := metricRows("total", m["tile"])
			reps := asMap(m["replicas"])
			for _, name := range slices.Sorted(maps.Keys(reps)) {
				rows = append(rows, metricRows(name, reps[name])...)
			}
			a.table(metricCols, rows)
			return nil
		}))
	rangeFlag(c, &rng)
	c.Example = "  stackr tile metrics api --stack shop --env prod --range 24h"
	return c
}
