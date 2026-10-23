package store

import (
	"context"
	"time"
)

// Credential is a row of credentials: registry pull creds for one org.
type Credential struct {
	ID        string    `db:"id" json:"id"`
	OrgID     string    `db:"org_id" json:"org_id"`
	Name      string    `db:"name" json:"name"`
	URL       string    `db:"url" json:"url"`
	Username  string    `db:"username" json:"username"`
	Password  string    `db:"password" json:"-"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type CredentialStore interface {
	Create(ctx context.Context, c Credential) error
	Get(ctx context.Context, id string) (Credential, error)
	ListByOrg(ctx context.Context, orgID string) ([]Credential, error)
	Update(ctx context.Context, c Credential) error
	Delete(ctx context.Context, id string) error
}

var credentialsT = newTable("credentials", func(c *Credential) []*string { return []*string{&c.Password} })

type credentials struct{ crud[Credential] }

func (s credentials) ListByOrg(ctx context.Context, orgID string) ([]Credential, error) {
	return s.many(ctx, "org_id = ?", orgID)
}

// Connector is a row of connectors: one connection of a kind, an org's or
// the server's (OrgID nil), which the admin shares with named orgs
// (connector_shares) or all of them (ShareAll). Settings is the kind's
// sealed blob. Host is the host a git connection serves (its Git source
// says); any other kind holds its kind there.
type Connector struct {
	ID        string    `db:"id" json:"id"`
	OrgID     *string   `db:"org_id" json:"org_id"`
	Kind      string    `db:"kind" json:"kind"`
	Name      string    `db:"name" json:"name"`
	Host      string    `db:"host" json:"host"`
	Settings  string    `db:"settings" json:"-"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	ShareAll  bool      `db:"share_all" json:"share_all"`
	// Capabilities is what the connector does: "git", "dns".
	Capabilities StringList `db:"capabilities" json:"capabilities"`
	// CheckedAt is when the core last asked the provider (nil = never),
	// CheckError what it refused then ("" = working).
	CheckedAt  *time.Time `db:"checked_at" json:"checked_at"`
	CheckError string     `db:"check_error" json:"check_error"`
	// Shared marks a server connector in an org's list; not a column.
	Shared bool `db:"-" json:"shared"`
	// Pending: its setup is out at the provider; not a column.
	Pending bool `db:"-" json:"pending"`
}

type ConnectorStore interface {
	Create(ctx context.Context, c Connector) error
	Get(ctx context.Context, id string) (Connector, error)
	GetByHost(ctx context.Context, orgID, host string) (Connector, error)
	ListByOrg(ctx context.Context, orgID string) ([]Connector, error)
	// ListAll is every connector, org and server.
	ListAll(ctx context.Context) ([]Connector, error)
	// ListServer is the server connectors (no org), by name.
	ListServer(ctx context.Context) ([]Connector, error)
	// ListSharedWith is the server connectors an org may read: shared with
	// all orgs, or with this one by name.
	ListSharedWith(ctx context.Context, orgID string) ([]Connector, error)
	// ShareOrgs is the org ids a server connector is shared with by name
	// (share_all is the row's own flag).
	ShareOrgs(ctx context.Context, id string) ([]string, error)
	// SetShares replaces a connector's named orgs.
	SetShares(ctx context.Context, id string, orgIDs []string) error
	// SetShareAll, SetName and SetApp write the one thing that changed, so
	// a stale read cannot put back columns another writer just set.
	SetShareAll(ctx context.Context, id string, all bool) error
	SetName(ctx context.Context, id, name string) error
	// SetApp is the finished handshake: the name and the (sealed) settings.
	SetApp(ctx context.Context, id, name, settings string) error
	// SetSettings rewrites the settings blob only.
	SetSettings(ctx context.Context, id, settings string) error
	// SetCheck records a check of the provider: when, and its refusal.
	SetCheck(ctx context.Context, id string, at time.Time, refusal string) error
	Update(ctx context.Context, c Connector) error
	Delete(ctx context.Context, id string) error
}

var connectorsT = newTable("connectors", func(c *Connector) []*string { return []*string{&c.Settings} })

type connectors struct{ crud[Connector] }

func (s connectors) GetByHost(ctx context.Context, orgID, host string) (Connector, error) {
	return s.one(ctx, "org_id = ? AND host = ?", orgID, host)
}

func (s connectors) SetShareAll(ctx context.Context, id string, all bool) error {
	res, err := s.q.ExecContext(ctx, "UPDATE connectors SET share_all = ? WHERE id = ?", all, id)
	return affected(res, err)
}

func (s connectors) SetName(ctx context.Context, id, name string) error {
	res, err := s.q.ExecContext(ctx, "UPDATE connectors SET name = ? WHERE id = ?", name, id)
	return affected(res, err)
}

func (s connectors) SetApp(ctx context.Context, id, name, settings string) error {
	row := Connector{Settings: settings}
	if err := s.seal(&row); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, "UPDATE connectors SET name = ?, settings = ? WHERE id = ?", name, row.Settings, id)
	return affected(res, err)
}

func (s connectors) SetSettings(ctx context.Context, id, settings string) error {
	row := Connector{Settings: settings}
	if err := s.seal(&row); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, "UPDATE connectors SET settings = ? WHERE id = ?", row.Settings, id)
	return affected(res, err)
}

func (s connectors) SetCheck(ctx context.Context, id string, at time.Time, refusal string) error {
	res, err := s.q.ExecContext(ctx, "UPDATE connectors SET checked_at = ?, check_error = ? WHERE id = ?", at, refusal, id)
	return affected(res, err)
}

func (s connectors) ListByOrg(ctx context.Context, orgID string) ([]Connector, error) {
	return s.many(ctx, "org_id = ?", orgID)
}

func (s connectors) ListAll(ctx context.Context) ([]Connector, error) {
	return s.many(ctx, "1 = 1 ORDER BY id")
}

func (s connectors) ListServer(ctx context.Context) ([]Connector, error) {
	return s.many(ctx, "org_id IS NULL ORDER BY name")
}

func (s connectors) ListSharedWith(ctx context.Context, orgID string) ([]Connector, error) {
	return s.many(ctx, `org_id IS NULL AND (share_all = 1 OR id IN
		(SELECT connector_id FROM connector_shares WHERE org_id = ?)) ORDER BY name`, orgID)
}

func (s connectors) ShareOrgs(ctx context.Context, id string) ([]string, error) {
	var ids []string
	err := s.q.SelectContext(ctx, &ids, "SELECT org_id FROM connector_shares WHERE connector_id = ? ORDER BY org_id", id)
	return ids, mapErr(err)
}

// SetShares clears then inserts, so a crash between the two leaves fewer
// shares, never more. ponytail: not one statement; a transaction when a
// caller needs the swap atomic.
func (s connectors) SetShares(ctx context.Context, id string, orgIDs []string) error {
	if _, err := s.q.ExecContext(ctx, "DELETE FROM connector_shares WHERE connector_id = ?", id); err != nil {
		return mapErr(err)
	}
	for _, o := range orgIDs {
		if _, err := s.q.ExecContext(
			ctx,
			"INSERT OR IGNORE INTO connector_shares (connector_id, org_id) VALUES (?, ?)",
			id,
			o,
		); err != nil {
			return mapErr(err)
		}
	}
	return nil
}
