// Package user owns the users table, and closes a user's sessions and keys
// when their standing drops (B16).
package user

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct {
	users    store.UserStore
	sessions store.SessionStore
	keys     store.APIKeyStore
}

func New(users store.UserStore, sessions store.SessionStore, keys store.APIKeyStore) *Leaf {
	return &Leaf{users: users, sessions: sessions, keys: keys}
}

// ErrBadLogin is one answer for unknown email, wrong password and disabled
// account, so the form cannot be used to probe which emails exist.
var ErrBadLogin = errs.Invalidf("", "invalid email or password")

func (l *Leaf) Get(ctx context.Context, id string) (store.User, error) {
	return l.users.Get(ctx, id)
}

// MinPassword is the one password rule ("Auth mechanics": minimum length only).
const MinPassword = 8

func hashPassword(pw string) (string, error) {
	if len(pw) < MinPassword {
		return "", errs.Invalidf("password", "must be at least %d characters", MinPassword)
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return h, nil
}

// Register creates an active user. The first account on a fresh install
// becomes the stackr admin.
// ponytail: two concurrent first registrations can both become admin; the
// installer's setup URL is opened once.
func (l *Leaf) Register(ctx context.Context, email, password, name string) (store.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if _, err := l.users.GetByEmail(ctx, email); err == nil {
		return store.User{}, errs.Invalidf("email", "an account with this email already exists")
	} else if !errors.Is(err, errs.ErrNotFound) {
		return store.User{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	all, err := l.users.List(ctx)
	if err != nil {
		return store.User{}, err
	}
	role := "user"
	if len(all) == 0 {
		role = "admin"
	}
	now := time.Now().UTC()
	u := store.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: hash,
		Name:         name,
		Role:         role,
		Active:       true,
		Theme:        "system",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	return u, l.users.Create(ctx, u)
}

// List is every account (admin screens).
func (l *Leaf) List(ctx context.Context) ([]store.User, error) { return l.users.List(ctx) }

// ChangePassword is the user's own change: it needs the current password.
func (l *Leaf) ChangePassword(ctx context.Context, id, current, next string) error {
	u, err := l.users.Get(ctx, id)
	if err != nil {
		return err
	}
	if ok, err := auth.CheckPassword(current, u.PasswordHash); err != nil {
		return fmt.Errorf("check password: %w", err)
	} else if !ok {
		return errs.Invalidf("current_password", "the current password is wrong")
	}
	return l.SetPassword(ctx, id, next)
}

// SetPassword is the admin's reset (no reset flow in v1).
func (l *Leaf) SetPassword(ctx context.Context, id, next string) error {
	hash, err := hashPassword(next)
	if err != nil {
		return err
	}
	return l.update(ctx, id, func(u *store.User) { u.PasswordHash = hash })
}

// Authenticate checks the password of an active user.
func (l *Leaf) Authenticate(ctx context.Context, email, password string) (store.User, error) {
	u, err := l.users.GetByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if errors.Is(err, errs.ErrNotFound) {
		return store.User{}, ErrBadLogin
	}
	if err != nil {
		return store.User{}, err
	}
	ok, err := auth.CheckPassword(password, u.PasswordHash)
	if err != nil {
		return store.User{}, fmt.Errorf("check password: %w", err)
	}
	if !ok || !u.Active {
		return store.User{}, ErrBadLogin
	}
	return u, nil
}

// HashToken is how an API key's secret is stored: never the token itself.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ByKey finds the key behind a bearer token and its owner, live.
func (l *Leaf) ByKey(ctx context.Context, token string) (store.User, store.APIKey, error) {
	k, err := l.keys.GetByHash(ctx, HashToken(token))
	if err != nil {
		return store.User{}, store.APIKey{}, err
	}
	u, err := l.users.Get(ctx, k.UserID)
	return u, k, err
}

// MintKey creates an API key and returns its bearer token, shown once; only
// the hash is stored. A key never holds more than its minter (B36): it is
// bound to orgID, where the minter must hold roleInOrg (the caller reads the
// membership); an unbound key (orgID "") is admin-only (DECIDE 13).
func (l *Leaf) MintKey(ctx context.Context, u store.User, orgID, roleInOrg, name string) (string, store.APIKey, error) {
	admin := u.Role == "admin"
	switch {
	case !u.Active:
		return "", store.APIKey{}, errs.ErrRefused
	case orgID == "" && !admin:
		return "", store.APIKey{}, errs.Refusedf("only a stackr admin can mint a key bound to no organization")
	case orgID != "" && roleInOrg == "" && !admin:
		return "", store.APIKey{}, errs.ErrNotFound
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", store.APIKey{}, err
	}
	token := hex.EncodeToString(b)
	k := store.APIKey{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		Name:      strings.TrimSpace(name),
		TokenHash: HashToken(token),
		CreatedAt: time.Now().UTC(),
	}
	if orgID != "" {
		k.OrgID = &orgID
	}
	return token, k, l.keys.Create(ctx, k)
}

// Keys lists a user's keys (hashes only).
func (l *Leaf) Keys(ctx context.Context, userID string) ([]store.APIKey, error) {
	return l.keys.ListByUser(ctx, userID)
}

// RevokeKey deletes one of the user's keys; someone else's is ErrNotFound.
func (l *Leaf) RevokeKey(ctx context.Context, userID, keyID string) error {
	k, err := l.keys.Get(ctx, keyID)
	if err != nil {
		return err
	}
	if k.UserID != userID {
		return errs.ErrNotFound
	}
	return l.keys.Delete(ctx, keyID)
}

// SetActive flips the account on or off. Disabling goes through the one
// "loses powers" rule.
func (l *Leaf) SetActive(ctx context.Context, id string, active bool) error {
	if !active {
		return l.losePowers(ctx, id, func(u *store.User) { u.Active = false })
	}
	return l.update(ctx, id, func(u *store.User) { u.Active = true })
}

// SetAdmin grants or takes the stackr admin role. Taking it goes through the
// one "loses powers" rule.
func (l *Leaf) SetAdmin(ctx context.Context, id string, admin bool) error {
	if !admin {
		return l.losePowers(ctx, id, func(u *store.User) { u.Role = "user" })
	}
	return l.update(ctx, id, func(u *store.User) { u.Role = "admin" })
}

// losePowers is B16: one rule for demote and disable. The install's last
// active admin cannot lose its powers (nobody could give them back); anyone
// else gets the change and every session and key closed.
func (l *Leaf) losePowers(ctx context.Context, id string, change func(*store.User)) error {
	u, err := l.users.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == "admin" && u.Active {
		all, err := l.users.List(ctx)
		if err != nil {
			return err
		}
		others := 0
		for _, o := range all {
			if o.ID != id && o.Role == "admin" && o.Active {
				others++
			}
		}
		if others == 0 {
			return errs.Conflictf("the last stackr admin cannot be demoted or disabled")
		}
	}
	change(&u)
	u.UpdatedAt = time.Now().UTC()
	if err := l.users.Update(ctx, u); err != nil {
		return err
	}
	return l.CloseAccess(ctx, id, "")
}

func (l *Leaf) update(ctx context.Context, id string, f func(*store.User)) error {
	u, err := l.users.Get(ctx, id)
	if err != nil {
		return err
	}
	f(&u)
	u.UpdatedAt = time.Now().UTC()
	return l.users.Update(ctx, u)
}

// CloseAccess is the one revoke function (B16): it ends every session of
// the user and deletes their keys bound to orgID, or all their keys when
// orgID is "". Both are attempted; the errors are joined.
func (l *Leaf) CloseAccess(ctx context.Context, userID, orgID string) error {
	var org *string
	if orgID != "" {
		org = &orgID
	}
	return errors.Join(
		l.sessions.DeleteBySubjectID(ctx, userID),
		l.keys.DeleteByUser(ctx, userID, org),
	)
}
