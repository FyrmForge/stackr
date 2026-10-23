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
	"errors"
	"net/http"
	"net/url"

	"github.com/libdns/libdns"
)

// Capabilities, as stored in connectors.capabilities.
const (
	CapDNS = "dns"
	CapGit = "git"
)

// The panel routes the core owns. A provider reaches a connection on them.
const (
	// CallbackPath is the one route a redirect setup comes back to.
	CallbackPath = "/-/connectors/callback"
	// HookPath + a connection's id is where its provider delivers events.
	HookPath = "/hooks/connectors/"
)

// ReturnPath is where a provider sends the browser back to connection id
// of kind after a step on its own site (an install), with the state it
// was given.
func ReturnPath(kind, id string) string { return "/settings/" + kind + "/installed/" + id }

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
	// Multiline: a text area (a pasted key).
	Multiline bool `json:"multiline,omitempty"`
}

// Prefills a Field may ask for.
const (
	// ServerIPv4 is the controller's public IPv4, the address a provider
	// sees stackr's calls come from.
	ServerIPv4 = "server_ipv4"
)

// Alter is a kind with a second way to set up, a plain form (GitHub: an
// App made outside stackr). Its values go to the same Setup.
type Alter interface {
	Alt() Alt
}

// Alt is that second way: the link to it and its steps.
type Alt struct {
	Label string `json:"label"`
	Steps []Step `json:"steps"`
}

// Redirector is a connection whose setup leaves for the provider and comes
// back through the core callback route.
type Redirector interface {
	// Out is where the browser goes. back is the callback URL, state the
	// value it must come back with; id is the connection's id-to-be.
	Out(ctx context.Context, id, back, state string) (Out, error)
	// Back completes the connection from the callback's query.
	Back(ctx context.Context, q url.Values) error
}

// Settled is a redirect kind's connection that its Setup completed
// outright (the Alt form): it is saved without going out.
type Settled interface {
	Settled() bool
}

// Valuer is a connection that says its setup fields' values (secrets too,
// which the core never shows), so its credentials can be updated in
// place: a secret left empty keeps the stored one. A Redirector updates
// through its Alt form.
type Valuer interface {
	Values() map[string]string
}

// Installer is a connection installed on the provider's side once saved.
// Install is the page that installs or manages it; state comes back with
// the browser to ReturnPath.
type Installer interface {
	Install(state string) string
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

// GitSource is the Git source capability: a git host account.
type GitSource interface {
	// Host is the host its repositories are on; a repository URL on it
	// is this account's to clone.
	Host() string
	// RepoURL is raw in the one stored spelling of a repository this
	// account serves (a short form or a URL); false when it is not one.
	RepoURL(raw string) (string, bool)
	// Repos is the repositories the account may read (the picker); none,
	// no error, while it is installed nowhere.
	Repos(ctx context.Context) ([]Repo, error)
	// Branches and YAMLFiles are a repository's, for the config picker.
	Branches(ctx context.Context, repo string) ([]string, error)
	YAMLFiles(ctx context.Context, repo, ref string) ([]string, error)
	// CloneEnv is the environment a fetch of repoURL authenticates with:
	// environment, never argv, so a token never lands in a process list.
	CloneEnv(ctx context.Context, repoURL string) ([]string, error)
	// Hook checks and decodes an event delivered to HookPath:
	// ErrBadSignature when it does not prove it is the provider's,
	// ErrBadPayload when it is not what it says. One nobody acts on is
	// the zero Event.
	Hook(ctx context.Context, header http.Header, body []byte) (Event, error)
	// SetHook points the provider's deliveries at url.
	SetHook(ctx context.Context, url string) error
}

// Hook's refusals: 401 and 400.
var (
	ErrBadSignature = errors.New("bad signature")
	ErrBadPayload   = errors.New("bad payload")
)

// Repo is a repository on a git account: FullName is owner/name, URL its
// stored spelling.
type Repo struct {
	FullName      string `json:"full_name"`
	URL           string `json:"url"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// Event is what a delivery reports: at most one of Push and PR.
type Event struct {
	Push *Push
	PR   *PullRequest
}

// Push is "repo X, branch Y, commit Z". Changed is every path it touched;
// nil when the provider did not say, which means anything may have changed.
type Push struct {
	Repo, Branch, Commit, Message, DefaultBranch string
	Changed                                      []string
}

// PullRequest is one opened, reopened, synchronize(d) or closed: Head at
// SHA onto Base. Fork: the head lives in another repository.
type PullRequest struct {
	Repo, Action    string
	Number          int
	Head, SHA, Base string
	Fork            bool
}

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
