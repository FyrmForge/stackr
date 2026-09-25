package settings

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Leaf reads and writes the settings table: the flat knobs and the server
// rung of the cascade.
type Leaf struct {
	rows store.SettingStore
	boot map[string]string
}

// New takes the boot values from service.Config. Precedence, in this one
// place (DECIDE 15): a settings row wins, then a boot value, then the
// catalogue default.
func New(rows store.SettingStore, boot map[string]string) *Leaf {
	return &Leaf{rows: rows, boot: boot}
}

// Get returns a flat knob's effective value as text.
func (l *Leaf) Get(ctx context.Context, key string) (string, error) {
	k, ok := lookup(key)
	if !ok || k.Scopes != Flat {
		return "", fmt.Errorf("no install setting named %q", key)
	}
	v, set, err := l.rows.Get(ctx, key)
	if err != nil || set {
		return v, err
	}
	if b, ok := l.boot[key]; ok {
		return b, nil
	}
	return k.Default, nil
}

// Int is Get for an int knob.
func (l *Leaf) Int(ctx context.Context, key string) (int, error) {
	v, err := l.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// Set writes a flat knob, refusing what does not parse (B24) and a read-only
// knob outright. The empty string removes the row, falling back to the boot
// value or default.
func (l *Leaf) Set(ctx context.Context, key, raw string) error {
	k, ok := lookup(key)
	if !ok || k.Scopes != Flat {
		return errs.Invalidf(key, "no install setting named %q", key)
	}
	if k.ReadOnly {
		return errs.Invalidf(key, "%s comes from the installer and cannot be changed here", key)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return l.rows.Delete(ctx, key)
	}
	if err := validate(k, raw); err != nil {
		return errs.Invalidf(key, "%s", err.Error())
	}
	return l.rows.Set(ctx, key, raw)
}

func validate(k Knob, raw string) error {
	switch k.Type {
	case TInt:
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%q is not a whole number", raw)
		}
		return usable(k, v)
	case TFloat:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", raw)
		}
		return usable(k, v)
	case TBool:
		if _, err := strconv.ParseBool(raw); err != nil {
			return fmt.Errorf("%q is not true or false", raw)
		}
	}
	return nil
}

// Defaults is the server rung of the cascade.
func (l *Leaf) Defaults(ctx context.Context) (Settings, error) {
	v, _, err := l.rows.Get(ctx, DefaultsKey)
	if err != nil {
		return Settings{}, err
	}
	return Parse(v)
}

// SetDefaults merges submitted values into the server rung.
func (l *Leaf) SetDefaults(ctx context.Context, vals map[string]string) error {
	cur, err := l.Defaults(ctx)
	if err != nil {
		return err
	}
	next, err := Merge(cur, vals, Server)
	if err != nil {
		return errs.Invalidf("", "%s", err.Error())
	}
	return l.rows.Set(ctx, DefaultsKey, next.JSON())
}
