package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Settings is one level's overrides. nil = inherit; a pointer is what lets
// an explicit 0 survive the cascade and the JSON round trip.
type Settings struct {
	CPULimit        *float64 `json:"cpu_limit,omitempty"`
	MemLimitMB      *int     `json:"mem_limit_mb,omitempty"`
	Protect         *bool    `json:"protect,omitempty"`
	ProtectUser     *string  `json:"protect_user,omitempty"`
	ProtectPassword *string  `json:"protect_password,omitempty"`
}

// Resolved is the cascaded result, every field concrete.
type Resolved struct {
	CPULimit        float64
	MemLimitMB      int
	Protect         bool
	ProtectUser     string
	ProtectPassword string
}

// Parse decodes a stored blob. Empty is no overrides; a corrupt blob is an
// error, never "sets nothing" (for Protect that would publish a URL).
func Parse(blob string) (Settings, error) {
	var s Settings
	if strings.TrimSpace(blob) == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(blob), &s); err != nil {
		return s, fmt.Errorf("settings: %w", err)
	}
	return s, nil
}

func (s Settings) JSON() string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Resolve applies the levels in order (server, org, stack, env); a later
// level wins where set, unset falls through to the catalogue default.
func Resolve(levels ...Settings) Resolved {
	var r Resolved // every cascade default is the zero value
	for _, s := range levels {
		if s.CPULimit != nil {
			r.CPULimit = *s.CPULimit
		}
		if s.MemLimitMB != nil {
			r.MemLimitMB = *s.MemLimitMB
		}
		if s.Protect != nil {
			r.Protect = *s.Protect
		}
		// One unit: a user from one level must never pair with a password
		// from another, or nobody can log in.
		if s.ProtectUser != nil || s.ProtectPassword != nil {
			r.ProtectUser, r.ProtectPassword = deref(s.ProtectUser), deref(s.ProtectPassword)
		}
	}
	return r
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// EffectiveLimits folds a tile's own limits (the fifth rung) over the
// resolved ones; a tile's zero means inherit.
func (r Resolved) EffectiveLimits(cpu float64, memMB int) (float64, int) {
	if cpu == 0 {
		cpu = r.CPULimit
	}
	if memMB == 0 {
		memMB = r.MemLimitMB
	}
	return cpu, memMB
}

// Set applies one submitted value to one field: the only string → field
// door. B24: a value that does not parse is refused, never read as unset.
// The empty string clears to inherit.
func Set(s *Settings, key, raw string) error {
	k, ok := lookup(key)
	if !ok || k.Scopes&Flat != 0 {
		return fmt.Errorf("no setting named %q", key)
	}
	raw = strings.TrimSpace(raw)
	switch key {
	case "cpu_limit":
		return setNum(&s.CPULimit, k, raw, parseFloat)
	case "mem_limit_mb":
		return setNum(&s.MemLimitMB, k, raw, strconv.Atoi)
	case "protect":
		return setNum(&s.Protect, k, raw, strconv.ParseBool)
	case "protect_user":
		s.ProtectUser = str(raw)
	case "protect_password":
		s.ProtectPassword = str(raw)
	}
	return nil
}

func parseFloat(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

func str(raw string) *string {
	if raw == "" {
		return nil
	}
	return &raw
}

// setNum parses raw into dst. Refused: parse failure, negative, and zero
// where the knob has no meaningful zero.
func setNum[T int | float64 | bool](dst **T, k Knob, raw string, parse func(string) (T, error)) error {
	if raw == "" {
		*dst = nil
		return nil
	}
	v, err := parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %q is not a valid %s", k.Key, raw, k.Type)
	}
	if err := usable(k, v); err != nil {
		return err
	}
	*dst = &v
	return nil
}

func usable[T int | float64 | bool](k Knob, v T) error {
	var f float64
	switch x := any(v).(type) {
	case int:
		f = float64(x)
	case float64:
		f = x
	default:
		return nil
	}
	if f < 0 || (f == 0 && !k.AllowZero) {
		return fmt.Errorf("%s: %v is not a usable value", k.Key, v)
	}
	return nil
}

// Merge folds submitted values into a level. Keys not submitted are left
// alone; all-or-nothing, so a refused save never half-applies.
func Merge(s Settings, vals map[string]string, at Scope) (Settings, error) {
	out := s
	for _, k := range Catalogue {
		raw, ok := vals[k.Key]
		if !ok {
			continue
		}
		if k.Scopes&at == 0 {
			return s, fmt.Errorf("%s cannot be set on a %s", k.Key, at)
		}
		if err := Set(&out, k.Key, raw); err != nil {
			return s, err
		}
	}
	for key := range vals {
		if _, ok := lookup(key); !ok {
			return s, fmt.Errorf("no setting named %q", key)
		}
	}
	if err := out.Check(); err != nil {
		return s, err
	}
	return out, nil
}

// Check refuses half a basic-auth pair: Resolve takes the pair as one unit.
func (s Settings) Check() error {
	user := s.ProtectUser != nil && *s.ProtectUser != ""
	pass := s.ProtectPassword != nil && *s.ProtectPassword != ""
	switch {
	case user == pass:
		return nil
	case user:
		return errors.New("protection needs a password as well as a user")
	default:
		return errors.New("protection needs a user as well as a password")
	}
}
