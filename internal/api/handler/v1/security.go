package v1

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// SecurityEventOut is one audited act: a login, a key, an elevated grant or a
// secret reveal.
type SecurityEventOut = service.SecurityEvent

// SecurityLog (server admin) lists the security log, newest first. Filters:
// ?action=, ?actor= (user id), ?target_kind= and ?target=, ?since= and
// ?until= (RFC 3339), ?limit=.
func (h *H) SecurityLog() Endpoint {
	return Get(func(c echo.Context) ([]SecurityEventOut, error) {
		f := service.SecurityFilter{
			Action:     c.QueryParam("action"),
			ActorID:    c.QueryParam("actor"),
			TargetKind: c.QueryParam("target_kind"),
			TargetID:   c.QueryParam("target"),
		}
		for name, to := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
			if v := c.QueryParam(name); v != "" {
				t, err := time.Parse(time.RFC3339, v)
				if err != nil {
					return nil, errs.Invalidf(name, "must be an RFC 3339 time")
				}
				*to = t
			}
		}
		if err := echo.QueryParamsBinder(c).Int("limit", &f.Limit).BindError(); err != nil {
			return nil, err
		}
		return list(h.Orch.SecurityLog(rc(c), f))
	}).Q("action", "actor", "target_kind", "target", "since", "until", "limit")
}
