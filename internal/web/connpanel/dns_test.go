package connpanel

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/dns"
)

// A record the last sync found off the zone reads not live, with why.
func TestRecordsNotLive(t *testing.T) {
	rs := service.CustomRecords{Source: "managed by acme.dev", Records: []service.CustomRecord{
		{ID: "1", Host: "acme.dev", Type: "MX", Value: "10 mx.example.net", NotLive: "This domain uses Namecheap mail forwarding. Switch Mail Settings to Custom MX first."},
		{ID: "2", Host: "acme.dev", Type: "TXT", Value: "v=1", Written: true},
	}}
	var b strings.Builder
	if err := ui.Records(Records(rs, "acme.dev", "/add", "/rm", "#t")).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.String(), "Not live: this domain uses Namecheap mail forwarding."); n != 1 {
		t.Errorf("not live shown %d times:\n%s", n, b.String())
	}
}
