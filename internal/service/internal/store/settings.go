package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
)

// SettingStore is the install-wide key/value table. Keys are the settings
// catalogue's; this file does not know them.
type SettingStore interface {
	// Get answers ("", false, nil) for a key never set.
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	All(ctx context.Context) (map[string]string, error)
	// ListPrefix is every row whose key starts with prefix, key to value.
	ListPrefix(ctx context.Context, prefix string) (map[string]string, error)
	// GetSealed and SetSealed are Get and Set for a value kept encrypted.
	GetSealed(ctx context.Context, key string) (string, error)
	SetSealed(ctx context.Context, key, value string) error
}

type settings struct {
	q   querier
	box *secrets.Box
}

func (s settings) GetSealed(ctx context.Context, key string) (string, error) {
	v, ok, err := s.Get(ctx, key)
	if err != nil || !ok || v == "" {
		return "", err
	}
	return s.box.Decrypt(v)
}

func (s settings) SetSealed(ctx context.Context, key, value string) error {
	v, err := s.box.Encrypt(value)
	if err != nil {
		return err
	}
	return s.Set(ctx, key, v)
}

func (s settings) Get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := mapErr(s.q.GetContext(ctx, &v, `SELECT value FROM settings WHERE key = ?`, key))
	if errors.Is(err, errs.ErrNotFound) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s settings) Set(ctx context.Context, key, value string) error {
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func (s settings) Delete(ctx context.Context, key string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

func (s settings) All(ctx context.Context) (map[string]string, error) {
	return s.ListPrefix(ctx, "")
}

func (s settings) ListPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	var rows []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	// substr, not LIKE: a key's _ and % stay literal
	if err := s.q.SelectContext(ctx, &rows, `SELECT key, value FROM settings WHERE substr(key, 1, ?) = ?`, len(prefix), prefix); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m, nil
}

// resealSettings re-encrypts every sealed setting, the canary included.
func resealSettings(ctx context.Context, q querier, old, next *secrets.Box) error {
	all, err := settings{q, old}.All(ctx)
	if err != nil {
		return err
	}
	for k, v := range all {
		if !secrets.Sealed(v) {
			continue
		}
		pt, err := old.Decrypt(v)
		if err != nil {
			return fmt.Errorf("setting %s: %w", k, err)
		}
		if err := (settings{q, next}).SetSealed(ctx, k, pt); err != nil {
			return err
		}
	}
	return nil
}

// CanaryKey is the sealed setting that says which master key the database is
// sealed with, so a crash during a rotation can be told apart on the next boot.
const CanaryKey = "master_key_canary"

// CanaryValue is what the canary holds.
const CanaryValue = "stackr"
