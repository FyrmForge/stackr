// Package connectors is the contract every connector kind plugs into. A
// Kind says what it is called and how it is set up (Steps); New is one
// connection of it, whose JSON is the sealed settings blob of its row and
// whose methods are the capabilities the kind offers (DNS, GitSource).
// Features ask for a capability, never a kind.
//
// Kinds live in packages under this one and are registered in one file,
// connectors/all; nothing outside this tree imports a kind package
// (TestKindImports).
package connectors

import (
	"context"
	"net/url"

	"github.com/libdns/libdns"
)

// Capabilities, as stored in connectors.capabilities.
const (
	CapDNS = "dns"
	CapGit = "git"
)

// Kind is one connector kind.
type Kind interface {
	Name() string  // the stored kind, the URL and CLI word: lower case
	Label() string // what the screens call it
	Hint() string  // its line in the add dialog's picker
	Steps() []Step // setup, in order
	New() Conn     // an empty connection, for Setup or the settings blob
}

// Conn is one connection of a kind.
type Conn interface {
	// Setup takes the form steps' values by field name and checks their
	// shape: every bad field an errs.Invalid on it, joined.
	Setup(values map[string]string) error
	// Name is the connection's name as listed.
	Name() string
}

// Step is one screen of a kind's setup: a form step (Intro, List, Fields,
// Note), or with Out set the redirect out to the provider and back through
// the core callback route (the Conn is a Redirector).
type Step struct {
	Title  string   `json:"title"`           // the stepper's word
	Intro  string   `json:"intro,omitempty"` // a line on top
	List   []string `json:"list,omitempty"`  // numbered instructions
	Fields []Field  `json:"fields,omitempty"`
	Note   string   `json:"note,omitempty"` // a line under the fields
	Out    bool     `json:"out,omitempty"`
}

// Field is one value a step asks for.
type Field struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
	// Secret: a password input, never prefilled, never sent back.
	Secret   bool `json:"secret,omitempty"`
	Optional bool `json:"optional,omitempty"`
	// Prefill is a fact the core fills in: the form starts with it and an
	// empty value on add takes it. "" = none.
	Prefill string `json:"prefill,omitempty"`
}

// Prefills a Field may ask for.
const (
	// ServerIPv4 is the controller's public IPv4, the address a provider
	// sees stackr's calls come from.
	ServerIPv4 = "server_ipv4"
)

// Redirector is a connection whose setup leaves for the provider and comes
// back through the core callback route.
type Redirector interface {
	// Out is where the browser goes. back is the callback URL, state the
	// value it must come back with; id is the connection's id-to-be.
	Out(ctx context.Context, id, back, state string) (Out, error)
	// Back completes the connection from the callback's query.
	Back(ctx context.Context, q url.Values) error
}

// Out sends the browser to URL: a GET, or with Method POST a form posting
// Values (GitHub's manifest flow).
type Out struct {
	Method string
	URL    string
	Values map[string]string
}

// Zone is a domain on a DNS account. OurDNS: the provider serves its DNS;
// false is "DNS elsewhere".
type Zone struct {
	Name   string
	OurDNS bool
}

// DNS is the DNS capability: one provider account. Record names are
// relative to the zone, libdns style; TakeOver points the zone at the
// provider's own DNS. A refusal comes back explained: an errs.Invalid on
// the field at fault, or an errs.Refused naming the cause.
type DNS interface {
	Zones(ctx context.Context) ([]Zone, error)
	Get(ctx context.Context, zone string) ([]libdns.Record, error)
	Append(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error)
	Set(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error)
	Delete(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error)
	TakeOver(ctx context.Context, zone string) error
}

// GitSource is the Git source capability: a git host account. Declared for
// phase 2, when GitHub moves onto it; nothing implements it yet.
type GitSource interface {
	// Repos is the repositories the account may read (the picker).
	Repos(ctx context.Context) ([]Repo, error)
	// RepoURL normalises a URL this account serves; false when it is not one.
	RepoURL(raw string) (string, bool)
	// CloneEnv is the environment a fetch of repoURL authenticates with.
	CloneEnv(ctx context.Context, repoURL string) ([]string, error)
	// Push decodes a provider webhook into the push it reports; nil for
	// another event.
	Push(ctx context.Context, header map[string][]string, body []byte) (*Push, error)
}

// Repo is a repository on a git account.
type Repo struct{ Name, URL, DefaultBranch string }

// Push is "repo X, branch Y, commit Z".
type Push struct{ Repo, Branch, Commit string }

// Caps is the capabilities k offers: the ones its connection implements.
func Caps(k Kind) []string {
	var out []string
	c := k.New()
	if _, ok := c.(DNS); ok {
		out = append(out, CapDNS)
	}
	if _, ok := c.(GitSource); ok {
		out = append(out, CapGit)
	}
	return out
}

// Redirects says whether k's setup leaves for the provider.
func Redirects(k Kind) bool {
	for _, s := range k.Steps() {
		if s.Out {
			_, ok := k.New().(Redirector)
			return ok
		}
	}
	return false
}
