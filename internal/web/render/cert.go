package render

import (
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Cert is the certificate line of a host; recheck is the POST behind
// "Check again now", "" = no button.
func Cert(s service.CertState, ok bool, recheck string) c.CertView {
	v := c.CertView{Recheck: recheck}
	if ok {
		v.Status, v.Reason, v.Soon = s.Status, s.Reason, s.Soon
		if !s.NotAfter.IsZero() {
			v.Until = s.NotAfter.UTC().Format(time.DateOnly)
		}
	}
	return v
}
