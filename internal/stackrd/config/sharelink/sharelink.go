// Package sharelink runs one-shot secret exchanges with people who have no
// account: a drop box they fill in (values land straight in the encrypted
// variables store) or a share link that shows them existing values once.
//
// Two rules hold the whole design together. Every state change happens on a
// POST, so link previewers and browser prefetch can't burn a link by looking
// at it. And every burn is a claim, an UPDATE guarded on the current state,
// so two concurrent submits can never both go through.
package sharelink

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

var (
	// ErrDead covers every reason a link won't open, wrong token, expired,
	// burned, revoked, locked. The public pages must not tell a stranger which.
	ErrDead = errors.New("link is no longer available")
	// ErrPass is returned when the passphrase is missing or wrong; after
	// repo.MaxLinkAttempts the link locks and ErrDead takes over.
	ErrPass = errors.New("wrong passphrase")
)

// Links is the secret_links table's owner, as this package needs it.
//
// D-8 of the drift audit: this package minted links and RevokeService revoked
// them. One concept, two owners, and the mint side carried no rules — which
// is how a scope granted at mint time still binds against whatever org the
// cookie held. Moving the writes behind the service does not fix that; it
// puts the mint and the revoke in one place, which is where the fix goes.
//
// An interface rather than the service itself because service/ is built on
// this package.
type Links interface {
	Mint(ctx context.Context, l *repo.SecretLink) error
	ByHash(ctx context.Context, tokenHash string) (*repo.SecretLink, error)
	Claim(ctx context.Context, id, state string) (bool, error)
	BurnDrop(ctx context.Context, id string, vars []repo.Variable) (bool, error)
	Touch(ctx context.Context, id string, attempts int, openedAt sql.NullTime) error
}

// HashToken is the one-way mapping from the token in the URL to what we store.
// Same shape as API keys: the plaintext exists only in the link we hand out.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Mint stores a link and returns the token for its URL. The token is returned
// exactly once, only its hash is persisted, so a lost link can't be recovered.
func Mint(ctx context.Context, store repo.Store, links Links, l *repo.SecretLink, pass string) (string, error) {
	token := secrets.RandomHex(32)
	l.ID = uuid.New().String()
	l.TokenHash = HashToken(token)
	l.State = repo.LinkOpen
	l.CreatedAt = time.Now()
	if pass != "" {
		h, err := auth.HashPassword(pass)
		if err != nil {
			return "", err
		}
		l.PassHash = h
	}
	if l.Fields == "" {
		l.Fields = "[]"
	}
	if err := links.Mint(ctx, l); err != nil {
		return "", err
	}
	return token, nil
}

// EncodeFields is the writer side of SecretLink.Fields.
func EncodeFields(fields []repo.SecretLinkField) (string, error) {
	b, err := json.Marshal(fields)
	return string(b), err
}

// Open resolves a token to a live link. It is safe on GET: no state changes
// here, and a passphrase-protected link reports ErrPass without touching the
// row, so a prefetch can't spend an attempt.
func Open(ctx context.Context, links Links, token string) (*repo.SecretLink, error) {
	l, err := links.ByHash(ctx, HashToken(token))
	if err != nil {
		return nil, err
	}
	if l == nil || l.Dead(time.Now()) {
		return nil, ErrDead
	}
	return l, nil
}

// Unlock checks the passphrase on a link Open already returned, counting wrong
// answers and locking the link once they run out. POST only, it writes.
func Unlock(ctx context.Context, links Links, l *repo.SecretLink, pass string) error {
	if l.PassHash == "" {
		return nil
	}
	ok, err := auth.CheckPassword(pass, l.PassHash)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	l.Attempts++
	if l.Attempts >= repo.MaxLinkAttempts {
		if _, err := links.Claim(ctx, l.ID, repo.LinkLocked); err != nil {
			return err
		}
		return ErrDead
	}
	if err := links.Touch(ctx, l.ID, l.Attempts, l.OpenedAt); err != nil {
		return err
	}
	return ErrPass
}

// Submit burns a drop box and writes what was collected, atomically: the
// claimed burn and the variable writes are one transaction, so a losing
// double-submit writes nothing and a failed write un-burns the link.
func Submit(ctx context.Context, store repo.Store, links Links, l *repo.SecretLink, values map[string]string) error {
	if l.Kind != repo.LinkDrop {
		return ErrDead
	}
	fields := l.FieldList()
	// Every declared field must arrive filled: a half-filled drop box that
	// burned would leave the operator with no way to ask for the rest.
	for _, f := range fields {
		if values[f.Name] == "" {
			return errors.New("every field is required")
		}
	}
	// no replan is kicked off here, so a config-managed stack's
	// pending-changes view can sit stale until something else touches it.
	// Wire the replan hook through if that starts confusing operators.
	now := time.Now()
	vars := make([]repo.Variable, 0, len(fields))
	for _, f := range fields {
		vars = append(vars, repo.Variable{
			OwnerKind: l.OwnerKind,
			OwnerID:   l.OwnerID,
			Name:      f.Name,
			Value:     values[f.Name],
			Secret:    f.Secret,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	// Burn and writes are one transaction: if any write fails, the burn rolls
	// back too, and the sender's retained form can simply be resubmitted.
	won, err := links.BurnDrop(ctx, l.ID, vars)
	if err != nil {
		return err
	}
	if !won {
		return ErrDead
	}
	for _, v := range vars {
		audit.Record(ctx, store, "drop-link:"+l.ID, audit.Set, l.OwnerKind, l.OwnerID, v.Name)
	}
	return nil
}

// Reveal burns a share link and reads the values it points at. Values are read
// live, never copied at mint, so revoking a link leaves nothing to clean up.
// With a grace window the link stays readable until the window closes rather
// than dying on the first read.
func Reveal(ctx context.Context, store repo.Store, links Links, l *repo.SecretLink) ([]repo.Variable, error) {
	if l.Kind != repo.LinkShare {
		return nil, ErrDead
	}
	if l.WindowMinutes > 0 {
		if !l.OpenedAt.Valid {
			l.OpenedAt.Time, l.OpenedAt.Valid = time.Now(), true
			if err := links.Touch(ctx, l.ID, l.Attempts, l.OpenedAt); err != nil {
				return nil, err
			}
		}
	} else if won, err := links.Claim(ctx, l.ID, repo.LinkBurned); err != nil {
		return nil, err
	} else if !won {
		return nil, ErrDead
	}

	all, err := store.ListVariables(ctx, l.OwnerKind, l.OwnerID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]repo.Variable, len(all))
	for _, v := range all {
		byName[v.Name] = v
	}
	// a pointer to a since-deleted variable is skipped, not an error.
	var out []repo.Variable
	for _, f := range l.FieldList() {
		if v, ok := byName[f.Name]; ok {
			out = append(out, v)
			if v.Secret {
				audit.Record(ctx, store, "share-link:"+l.ID, audit.Share, l.OwnerKind, l.OwnerID, v.Name)
			}
		}
	}
	return out, nil
}

// Revoke kills a link whatever state it is in.
func Revoke(ctx context.Context, links Links, id string) error {
	_, err := links.Claim(ctx, id, repo.LinkRevoked)
	return err
}
