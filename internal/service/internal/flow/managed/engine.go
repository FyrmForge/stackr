// Package managed holds the managed-tile rules once, engine-blind: the
// ManagedTile interface and its registry, one slice per consumer, what
// happens to slices when a consumer or the instance goes. The instance
// container is a plain tile that flow/deploy runs; this package never starts
// a container. Engines run their work through Tools (DECIDE 7 a): postgres
// execs argv inside its container, s3 speaks the S3 API.
package managed

import (
	"context"
	"fmt"
	"strings"
)

// Exec runs argv inside the instance's container (leaf/tile.Exec).
type Exec func(ctx context.Context, cmd []string) (string, error)

// S3Admin is the bucket half of the s3 wrapper (s3.Admin).
type S3Admin interface {
	Ping(ctx context.Context) error
	CreateBucket(ctx context.Context, name string) error
	DropBucket(ctx context.Context, name string) error
	SetPublic(ctx context.Context, name string, public bool) error
}

// Tools is how an engine reaches its instance; the flow fills it per call.
type Tools struct {
	Exec Exec
	S3   S3Admin
}

// ManagedTile is one engine.
// ponytail: no Scale() yet; add it when an engine can actually scale.
type ManagedTile interface {
	Definition() Definition
	// Ready asks the instance itself, never "the container is running".
	Ready(ctx context.Context, i Instance, x Tools) error
	// Provision is idempotent: re-provisioning a slice re-syncs it.
	Provision(ctx context.Context, i Instance, s Slice, x Tools) error
	Drop(ctx context.Context, i Instance, s Slice, x Tools) error
	Bindings(i Instance, s Slice) []Binding
	// Backup is argv whose stdout is the dump; nil = the method is not offered.
	Backup(method string, i Instance) []string
	// Restore is argv that reads the dump on stdin and replaces target.
	Restore(method, target string, i Instance) []string
}

// Definition is what flow/deploy needs to run the instance as a tile.
type Definition struct {
	Image   string
	Command []string                  // nil = the image's entrypoint
	Config  func(i Instance) []string // first-boot env, credentials included
	Port    int
	Volumes []string // data paths that must survive a recreate

	PrimaryOutput string   // the binding a consumer gets when it names none
	InjectAll     bool     // the consumer needs the whole set
	PublicSlices  bool     // slices can be published read-only
	SliceNoun     string   // what the UI calls a slice
	Backups       []string // methods this engine offers
	AdminDB       string   // the engine's own namespace
	// SliceName normalises a base name; SliceSep joins the uniquifier.
	SliceName func(string) string
	SliceSep  string
}

// Instance is the running engine, as facts.
type Instance struct {
	Slug, Engine             string
	AdminUser, AdminPassword string
	AdminDB                  string
	Host                     string // the in-network alias consumers dial
	Port                     int
	PublicBase               string // scheme+host of its public domain, "" if none
}

// Slice is one consumer's cut: a logical db, a bucket.
type Slice struct {
	Name, User, Password string
	Public               bool
}

// Binding is one published connection detail.
type Binding struct {
	Name            string
	Value           string
	Secret          bool
	RequiresNetwork bool
}

// Engines is the registry: one line per engine.
var Engines = map[string]ManagedTile{
	"postgres": pg{},
	"s3":       s3e{},
}

func engine(name string) (ManagedTile, error) {
	e, ok := Engines[name]
	if !ok {
		return nil, fmt.Errorf("managed engine %q is not supported", name)
	}
	return e, nil
}

// sqlIdent: lowercase, digits and underscores, never leading with a digit.
func sqlIdent(slug string) string {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r == '-':
			return '_'
		}
		return -1
	}, strings.ToLower(slug))
	if id == "" || (id[0] >= '0' && id[0] <= '9') {
		id = "db_" + id
	}
	return id
}

// uniqueSliceName suffixes base until no existing slice answers to it.
func uniqueSliceName(base string, existing []string, sep string) string {
	taken := map[string]bool{}
	for _, n := range existing {
		taken[n] = true
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s%s%d", base, sep, i)
	}
	return name
}
