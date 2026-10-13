// Package securitylog owns security_events: the append only record of
// logins, keys, elevated grants and secret reveals. It only appends and
// reads; the orchestrator calls Append on a leaf bound to the same
// transaction as the act, so a failed append rolls the act back.
package securitylog

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Actions.
const (
	LoginOK       = "login.ok"
	LoginBad      = "login.bad"     // wrong password, unknown email or disabled account
	LoginLimited  = "login.limited" // refused by the rate limit
	KeyMint       = "key.mint"
	KeyRevoke     = "key.revoke"
	GrantApprove  = "grant.approve"
	GrantRevoke   = "grant.revoke"
	SecretReveal  = "secrets.reveal"
	NodeBootstrap = "node.bootstrap" // the controller row was made
	RegistryToken = "registry.token" // the registry front issued a token
	CertIssue     = "cert.issue"     // the cluster CA signed a certificate
	MoveStart     = "move.start"
	MoveFinish    = "move.finish"
	NodeRecover   = "node.recover"    // a worker fetched its mesh through the panel, or was refused
	NodeKeyRotate = "node.key_rotate" // a node's mesh and exit keys were replaced
	NodeCertRenew = "node.cert_renew" // a worker renewed its node cert (over the tunnel or the panel)
	RegistryClean = "registry.clean"  // a registry sweep finished (the schedule or "Clean up now")
)

// Event is what a caller records; Append stamps the id and time.
type Event = store.SecurityEvent

// Filter narrows List.
type Filter = store.SecurityFilter

type Leaf struct{ rows store.SecurityEventStore }

func New(rows store.SecurityEventStore) *Leaf { return &Leaf{rows: rows} }

// Append writes one event.
func (l *Leaf) Append(ctx context.Context, e Event) error {
	e.ID, e.At = uuid.NewString(), time.Now().UTC()
	return l.rows.Create(ctx, e)
}

// List is the matching events, newest first.
func (l *Leaf) List(ctx context.Context, f Filter) ([]Event, error) {
	return l.rows.Find(ctx, f)
}
