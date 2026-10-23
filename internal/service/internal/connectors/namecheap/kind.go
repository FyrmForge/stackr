// Package namecheap is the Namecheap connector kind: an account's API
// access, with the DNS capability.
package namecheap

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
)

// Kind is Namecheap.
type Kind struct{}

func (Kind) Name() string  { return "namecheap" }
func (Kind) Label() string { return "Namecheap" }
func (Kind) Hint() string {
	return "DNS records and wildcard certificates for your Namecheap domains."
}

func (Kind) New() connectors.Conn { return &Account{} }

// Steps 1 to 4 are one form posted at the last: the key never comes back.
func (Kind) Steps() []connectors.Step {
	return []connectors.Step{
		{
			Title: "API access",
			Intro: "Turn on API access in Namecheap.",
			List: []string{
				"Open Profile, Tools, Business & Dev Tools, Namecheap API Access.",
				"Turn API access on and copy the API key.",
			},
		},
		{Title: "Account", Fields: []connectors.Field{
			{Name: "user", Label: "Namecheap username"},
			{Name: "key", Label: "API key", Secret: true, Help: "Controls your whole Namecheap account."},
		}},
		{
			Title: "Allow list",
			Fields: []connectors.Field{{
				Name: "client_ip", Label: "Server IPv4", Prefill: connectors.ServerIPv4,
				Help: "Add this IP to Namecheap's API allow list.",
			}},
			Note: "Behind NAT, use your router's public IPv4.",
		},
		{Title: "Check", Intro: "stackr asks Namecheap for your domains to check the account."},
	}
}

// Account is one Namecheap account: its settings blob, and the DNS
// capability.
type Account struct {
	User     string `json:"user"`
	Key      string `json:"key"`
	ClientIP string `json:"client_ip"`

	endpoint string // "" = production; a test's server
}

var _ connectors.DNS = (*Account)(nil)

func (a *Account) Name() string { return "Namecheap · " + a.User }

// Setup takes user, key and client_ip; every missing one is named at once.
func (a *Account) Setup(v map[string]string) error {
	a.User, a.Key, a.ClientIP = strings.TrimSpace(v["user"]), v["key"], strings.TrimSpace(v["client_ip"])
	var bad []error
	if a.User == "" {
		bad = append(bad, errs.Invalidf("user", "Enter the Namecheap username"))
	}
	if a.Key == "" {
		bad = append(bad, errs.Invalidf("key", "Enter the API key"))
	}
	if ip, err := netip.ParseAddr(a.ClientIP); err != nil || !ip.Is4() {
		bad = append(bad, errs.Invalidf("client_ip", "Namecheap needs this server's public IPv4 address"))
	}
	return errors.Join(bad...)
}

// APIError is Namecheap's numbered refusal.
type APIError struct{ Number, Text string }

func (e *APIError) Error() string { return "namecheap error " + e.Number + ": " + e.Text }

// explain turns a refusal into the cause the form shows (Namecheap's
// global error codes).
func (a *Account) explain(err error) error {
	var ae *APIError
	if _, typed := errs.IsInvalid(err); err == nil || typed {
		return err
	}
	if !errors.As(err, &ae) {
		return errs.Refusedf("Namecheap did not answer: %v", err)
	}
	switch ae.Number {
	case "1011102", "1010102":
		return errs.Invalidf("key", "Namecheap refused the API key: it is wrong, or API access is not turned on for this account")
	case "1011150", "1017150", "1011105", "1017105", "1010105":
		return errs.Invalidf("client_ip", "Namecheap does not take API calls from %s; add it to the API allow list", a.ClientIP)
	case "1017101":
		return errs.Invalidf("user", "API access for %s is off or locked; turn it on under Namecheap API Access", a.User)
	case "1010101", "1011101", "1019103", "1016103", "1017103":
		return errs.Invalidf("user", "Namecheap does not know the API user %s", a.User)
	}
	return errs.Refusedf("Namecheap refused: %s (error %s)", ae.Text, ae.Number)
}

// The DNS capability: every call's refusal explained.

func (a *Account) Zones(ctx context.Context) ([]connectors.Zone, error) {
	zs, err := a.zones(ctx)
	return zs, a.explain(err)
}

func (a *Account) Get(ctx context.Context, zone string) ([]libdns.Record, error) {
	rs, err := a.get(ctx, zone)
	return rs, a.explain(err)
}

func (a *Account) Append(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	out, err := a.append(ctx, zone, rs)
	return out, a.explain(err)
}

func (a *Account) Set(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	out, err := a.set(ctx, zone, rs)
	return out, a.explain(err)
}

func (a *Account) Delete(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	out, err := a.delete(ctx, zone, rs)
	return out, a.explain(err)
}

func (a *Account) TakeOver(ctx context.Context, zone string) error {
	return a.explain(a.takeOver(ctx, zone))
}
