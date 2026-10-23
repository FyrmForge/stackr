package dnscap

import (
	"context"
	"errors"
	"strings"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Names is every managed domain, sorted.
func (l *Leaf) Names(ctx context.Context) ([]string, error) {
	rows, err := l.rows.List(ctx)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, err
}

// Challenge checks an ACME DNS-01 call from a proxy, which anything on the
// box could make: zone must be a managed domain and every record a TXT with
// a value named _acme-challenge or _acme-challenge.<labels>, relative to
// zone. An empty value would match every TXT on delete. The row's connector
// answers the call.
func (l *Leaf) Challenge(ctx context.Context, zone string, rs []libdns.RR) (store.DNSDomain, error) {
	if len(rs) == 0 {
		return store.DNSDomain{}, errs.Invalidf("records", "no records")
	}
	for _, r := range rs {
		ok := r.Name == "_acme-challenge" || strings.HasPrefix(r.Name, "_acme-challenge.") && !strings.HasSuffix(r.Name, ".")
		if !ok || !strings.EqualFold(r.Type, "TXT") || r.Data == "" {
			return store.DNSDomain{}, errs.Invalidf("records", "%s %s is not an ACME challenge record", r.Type, r.Name)
		}
	}
	row, err := l.rows.GetByName(ctx, Normal(zone))
	if errors.Is(err, errs.ErrNotFound) {
		return row, errs.Refusedf("%s is not a managed domain", Normal(zone))
	}
	return row, err
}
