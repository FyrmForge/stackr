package v1

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// metricsIn reads ?range= (default 1h, max 30 days) and ?step= (default
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
// rtt_ms, loss.
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
// io_read_bps, io_write_bps, net_rx_bps, net_tx_bps) and each replica's.
func (h *H) TileMetrics() Endpoint {
	return Get(func(c echo.Context) (service.TileMetricsOut, error) {
		in, err := metricsIn(c)
		if err != nil {
			return service.TileMetricsOut{}, err
		}
		return h.Orch.TileMetrics(rc(c), tileID(c), in)
	}).Q("range", "step")
}
