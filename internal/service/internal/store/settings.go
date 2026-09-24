package store

import (
	"context"
	"errors"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// SettingStore is the install-wide key/value table. Keys are the settings
// catalogue's; this file does not know them.
type SettingStore interface {
	// Get answers ("", false, nil) for a key never set.
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	All(ctx context.Context) (map[string]string, error)
}

type settings struct{ q querier }

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
	var rows []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	if err := s.q.SelectContext(ctx, &rows, `SELECT key, value FROM settings`); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m, nil
}
