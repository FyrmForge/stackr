// Package selfmetrics is stackr's own Prometheus metrics, served on GET
// /metrics: Go runtime and process, HTTP requests by route and status
// class, finished jobs by kind and outcome, and builds by outcome. It has
// its own registry, so the Caddy collectors linked into the same binary stay
// out.
package selfmetrics

import (
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	registry = prometheus.NewRegistry()
	slow     = prometheus.ExponentialBuckets(1, 2, 12) // 1 s to about 34 min

	httpDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "stackr_http_request_duration_seconds",
		Help: "HTTP requests by route pattern and status class (streams left out).",
	}, []string{"route", "status"})
	jobDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "stackr_job_duration_seconds", Buckets: slow,
		Help: "Finished jobs by kind (deploy, promote, ...) and outcome (done, failed, ...).",
	}, []string{"kind", "outcome"})
	buildDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "stackr_build_duration_seconds", Buckets: slow,
		Help: "Image builds of git tiles by outcome (ok, failed).",
	}, []string{"outcome"})

	reqs, errs5 atomic.Uint64
)

func init() {
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		httpDur, jobDur, buildDur)
}

// Handler serves the registry in the Prometheus text format, uncompressed:
// the server's own gzip compresses it once.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{DisableCompression: true})
}

// Middleware times each request by its route pattern; skip leaves out the
// long-lived ones (event streams, terminals), which would swamp the latency.
func Middleware(skip func(echo.Context) bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if skip(c) {
				return next(c)
			}
			start := time.Now()
			err := next(c)
			// An error is written after the middleware returns, so its
			// status comes from the error.
			code := c.Response().Status
			if err != nil {
				code = http.StatusInternalServerError
				var he *echo.HTTPError
				if errors.As(err, &he) {
					code = he.Code
				}
			}
			route := c.Path()
			if route == "" {
				route = "other"
			}
			httpDur.WithLabelValues(route, strconv.Itoa(code/100)+"xx").Observe(time.Since(start).Seconds())
			reqs.Add(1)
			if code >= 500 {
				errs5.Add(1)
			}
			return err
		}
	}
}

// HTTPTotals is every request timed so far and the 5xx among them.
func HTTPTotals() (all, err5 uint64) { return reqs.Load(), errs5.Load() }

// Job records a finished job.
func Job(kind, outcome string, took time.Duration) {
	jobDur.WithLabelValues(kind, outcome).Observe(took.Seconds())
}

// Build records a finished image build.
func Build(err error, took time.Duration) {
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	buildDur.WithLabelValues(outcome).Observe(took.Seconds())
}
