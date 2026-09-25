package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
)

// ServerSetting is one catalogue knob as the admin edits it: the install
// rows and the server rung of the cascade in one list. Value is what is
// set here (a cascade knob's "" = the default applies); DecidedBy says
// where Effective comes from. A secret's Value never leaves the service.
type ServerSetting struct {
	Knob
	Value     string
	Effective string
	DecidedBy string // install | server | default
	Secret    bool
}

// secretKnobs are shown as set or not, never echoed.
var secretKnobs = map[string]bool{"protect_password": true, "dns_env": true}

// ServerSettings is every knob the server may set, in catalogue order.
func (o *Orchestrator) ServerSettings(ctx context.Context) ([]ServerSetting, error) {
	d, err := o.settings.Defaults(ctx)
	if err != nil {
		return nil, err
	}
	var rung map[string]any
	if err := json.Unmarshal([]byte(d.JSON()), &rung); err != nil {
		return nil, err
	}
	var out []ServerSetting
	for _, k := range settings.Catalogue {
		s := ServerSetting{Knob: k, Secret: secretKnobs[k.Key]}
		switch {
		case k.Scopes&settings.Flat != 0:
			if s.Value, err = o.settings.Get(ctx, k.Key); err != nil {
				return nil, err
			}
			s.Effective, s.DecidedBy = s.Value, "install"
		case k.Scopes&settings.Server != 0:
			s.Effective, s.DecidedBy = k.Default, "default"
			if v, ok := rung[k.Key]; ok {
				s.Value = fmt.Sprint(v)
				s.Effective, s.DecidedBy = s.Value, "server"
			}
		default:
			continue
		}
		if s.Secret {
			s.Effective = "not set"
			if s.Value != "" {
				s.Effective = "set"
			}
			s.Value = ""
		}
		out = append(out, s)
	}
	return out, nil
}

// SetServerSettings writes only the knobs whose value changed: a cascade
// change redeploys every running tile (B34), so an untouched form must be a
// no-op. An empty secret keeps the stored one.
// ponytail: a secret cannot be cleared from here; the API's PUT does it.
func (o *Orchestrator) SetServerSettings(ctx context.Context, vals map[string]string) error {
	cur, err := o.ServerSettings(ctx)
	if err != nil {
		return err
	}
	cascade := map[string]string{}
	for _, s := range cur {
		raw, ok := vals[s.Key]
		if !ok || raw == s.Value || (s.Secret && raw == "") {
			continue
		}
		if s.Scopes&settings.Flat == 0 {
			cascade[s.Key] = raw
			continue
		}
		if err := o.SetSetting(ctx, s.Key, raw); err != nil {
			return err
		}
	}
	if len(cascade) == 0 {
		return nil
	}
	return o.SetSettingDefaults(ctx, cascade)
}
