package v1

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// metricsIn reads ?range= (default 1h, max 24h, or the history retention when on) and ?step= (default
// range/120), both Go durations like 15m or 24h.
func metricsIn(c echo.Context) (service.MetricsIn, error) {
	var in service.MetricsIn
	for name, d := range map[string]*time.Duration{"range": &in.Range, "step": &in.Step} {
		if v := c.QueryParam(name); v != "" {
			x, err := time.ParseDuration(v)
			if err != nil || x < 0 {
				return in, errs.Invalidf(name, "must be a duration like 15m or 24h")
			}
			*d = x
		}
	}
	return in, nil
}

// NodeMetrics (server admin) is a node's host series: cpu_pct, mem_used,
// mem_total, disk_used, disk_total.
func (h *H) NodeMetrics() Endpoint {
	return Get(func(c echo.Context) (service.Series, error) {
		in, err := metricsIn(c)
		if err != nil {
			return nil, err
		}
		return h.Orch.NodeMetrics(rc(c), c.Param("node"), in)
	}).Q("range", "step")
}

// LinkMetrics (server admin) is the tunnel between the node and ?peer=:
// rtt_ms, loss, rx_bps, tx_bps, handshake_age_s.
func (h *H) LinkMetrics() Endpoint {
	return Get(func(c echo.Context) (service.Series, error) {
		in, err := metricsIn(c)
		if err != nil {
			return nil, err
		}
		return h.Orch.LinkMetrics(rc(c), c.Param("node"), c.QueryParam("peer"), in)
	}).Q("peer", "range", "step")
}

// TileMetrics is a tile's series summed over its replicas (cpu_pct, mem,
// io_read_bps, io_write_bps, net_rx_bps, net_tx_bps) and each replica's,
// plus its HTTP traffic over every ingress node (req_s, 4xx_s, 5xx_s,
// latency_ms).
func (h *H) TileMetrics() Endpoint {
	return Get(func(c echo.Context) (service.TileMetricsOut, error) {
		in, err := metricsIn(c)
		if err != nil {
			return service.TileMetricsOut{}, err
		}
		return h.Orch.TileMetrics(rc(c), tileID(c), in)
	}).Q("range", "step")
}

// LogSearch is the env's stored log lines (the whole env, or one tile with
// ?tile=), newest first, 500 at most. ?kind=, ?source= (app, access, run,
// build), ?stream= (stdout, stderr), ?run= and ?text= narrow it; ?since= is a
// duration up to 168h (default 1h); ?before= is an RFC 3339 time, to page
// past the oldest line of the last answer.
func (h *H) LogSearch() Endpoint {
	return Get(func(c echo.Context) ([]service.LogLine, error) {
		q := c.QueryParam
		in := service.LogsIn{Tile: q("tile"), Kind: q("kind"), Source: q("source"), Stream: q("stream"), Run: q("run"), Text: q("text")}
		if v := q("since"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errs.Invalidf("since", "must be a duration like 15m or 24h")
			}
			in.Since = d
		}
		if v := q("before"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return nil, errs.Invalidf("before", "must be an RFC 3339 time")
			}
			in.Before = t
		}
		return list(h.Orch.SearchLogs(rc(c), scope(c), in))
	}).Q("tile", "kind", "source", "stream", "run", "text", "since", "before")
}
