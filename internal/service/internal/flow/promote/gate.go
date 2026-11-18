package promote

import (
	"errors"
	"fmt"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
)

// warnGate says what would lock a domain's basic auth, or leave its
// password readable: neither blocks the plan.
func warnGate(p *Plan, rr *params.Resolver, name, host string, a *domain.BasicAuth) {
	if a == nil {
		return
	}
	for _, v := range []string{a.User, a.Password} {
		_, plain, err := rr.ExpandGate(v)
		var unset errs.Unset
		switch {
		case errors.As(err, &unset):
			p.Warnings = append(p.Warnings, fmt.Sprintf("tile %s: %s stays locked until params.%s is set", name, host, unset.Param))
		case err != nil:
			p.Warnings = append(p.Warnings, fmt.Sprintf("tile %s: %s stays locked: %v", name, host, err))
		case plain && v == a.Password:
			p.Warnings = append(p.Warnings, fmt.Sprintf("tile %s: %s gate password is a plain variable, readable in the panel", name, host))
		}
	}
}
