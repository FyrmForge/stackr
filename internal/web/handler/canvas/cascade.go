package canvas

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
)

// cascadeKeys are the catalogue knobs an org, stack or env rung may set.
// ponytail: named here because the web layer cannot import the leaf's
// scope bits; a new cascade knob needs its line here and in parseRung.
var cascadeKeys = map[string]bool{
	"cpu_limit":        true,
	"mem_limit_mb":     true,
	"protect":          true,
	"protect_user":     true,
	"protect_password": true,
}

// rung is one level above the edited one: name as the Inherited line says
// it, tag as the From mark does ("org", "stack", "server").
type rung struct {
	name string // "server", "smoke organization"
	tag  string
	blob string
}

// decode reads a stored rung; empty sets nothing.
func decode(blob string) (map[string]any, error) {
	m := map[string]any{}
	if strings.TrimSpace(blob) == "" {
		return m, nil
	}
	err := json.Unmarshal([]byte(blob), &m)
	return m, err
}

func word(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// limitWord is how v0 says an inherited limit.
func limitWord(key, v string) string {
	switch {
	case key != "cpu_limit" && key != "mem_limit_mb":
		return v
	case v == "0" || v == "":
		return "no limit"
	case key == "cpu_limit":
		return v + " cores"
	}
	return v + " MB"
}

// cascadeForm is the card's own rung as the settings form (cascadeRows).
// Resolve's pair rule holds: the rung that sets the user or the password
// decides both. A stored password never leaves.
func (h *handler) cascadeForm(c echo.Context, cd card, own string) (comp.SettingsFormView, error) {
	s := cd.s
	v := comp.SettingsFormView{
		ID:     cd.kind + "-cascade",
		Action: urlOf(s) + "/-/drawer/settings",
		Scope:  cd.kind,
		Flat:   true,
	}
	server, err := h.orch.SettingDefaults(c.Request().Context())
	if err != nil {
		return v, err
	}
	above := []rung{{s.Org.Name + " organization", "org", s.Org.Settings}, {"server", "server", server.JSON()}}
	switch cd.kind {
	case "org":
		above = above[1:]
	case "env":
		above = append([]rung{{s.Stack.Name + " stack", "stack", s.Stack.Settings}}, above...)
	}
	v.Rows, err = cascadeRows(h.orch.Settings(), own, above)
	return v, err
}

// cascadeRows is one row per cascade knob: this rung's value (Changed when
// it sets one) and, left empty, what applies from the rungs above (nearest
// first) and which one decided it (From; "" = the built-in default).
func cascadeRows(knobs []service.Knob, own string, above []rung) ([]comp.SettingRowView, error) {
	mine, err := decode(own)
	if err != nil {
		return nil, err
	}
	ups := make([]map[string]any, len(above))
	for i, r := range above {
		if ups[i], err = decode(r.blob); err != nil {
			return nil, err
		}
	}
	var rows []comp.SettingRowView
	for _, k := range knobs {
		if !cascadeKeys[k.Key] {
			continue
		}
		r := comp.SettingRowView{
			Key:    k.Key,
			Label:  k.Label,
			Desc:   k.Desc,
			Type:   string(k.Type),
			Secret: k.Key == "protect_password",
		}
		if val, ok := mine[k.Key]; ok {
			r.Value, r.Changed = word(val), true
		}
		if r.Secret {
			r.Effective = "not set"
			if r.Value != "" {
				r.Effective = "set"
			}
			r.Value = ""
			rows = append(rows, r)
			continue
		}
		r.Effective = k.Default
		for i, up := range ups {
			val, ok := up[k.Key]
			if _, pw := up["protect_password"]; k.Key == "protect_user" && !ok && pw {
				val, ok = "", true
			}
			if ok {
				r.Effective, r.DecidedBy = word(val), above[i].name
				if !r.Changed {
					r.From = above[i].tag
				}
				break
			}
		}
		r.Effective = limitWord(k.Key, r.Effective)
		rows = append(rows, r)
	}
	return rows, nil
}

// cascadeSummary is a closed row's line for a cascade form: the limits and
// protection that apply here, an inherited one with where it comes from.
func cascadeSummary(rows []comp.SettingRowView) string {
	var parts []string
	for _, r := range rows {
		var s string
		switch r.Key {
		case "cpu_limit", "mem_limit_mb":
			s = r.Effective
			if r.Value != "" {
				s = limitWord(r.Key, r.Value)
			}
			if s == "no limit" {
				s = map[string]string{"cpu_limit": "no CPU limit", "mem_limit_mb": "no memory limit"}[r.Key]
			}
		case "protect":
			s = "no password"
			if cmp.Or(r.Value, r.Effective) == "true" {
				s = "password protected"
			}
		default:
			continue
		}
		if r.From != "" {
			s += " (from " + r.From + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

// parseRung folds the posted rows into the stored rung: blank inherits, a
// blank password keeps the stored one, clearing the user clears the pair.
// A refusal names its row. changed is false for an untouched form, so it
// redeploys nothing (B34).
func parseRung(stored string, form url.Values) (blob string, changed bool, err error) {
	var s service.SettingsBlob
	if strings.TrimSpace(stored) != "" {
		if err := json.Unmarshal([]byte(stored), &s); err != nil {
			return "", false, err
		}
	}
	before := s.JSON()
	get := func(k string) string { return strings.TrimSpace(form.Get(k)) }
	if s.CPULimit, err = limit("cpu_limit", get("cpu_limit"), func(r string) (float64, error) {
		return strconv.ParseFloat(r, 64)
	}); err != nil {
		return "", false, err
	}
	if s.MemLimitMB, err = limit("mem_limit_mb", get("mem_limit_mb"), strconv.Atoi); err != nil {
		return "", false, err
	}
	s.Protect = nil
	if raw := get("protect"); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			return "", false, errs.Invalidf("protect", "Choose on, off or inherit.")
		}
		s.Protect = &on
	}
	user := get("protect_user")
	s.ProtectUser = nil
	if user != "" {
		s.ProtectUser = &user
	}
	if pw := form.Get("protect_password"); pw != "" {
		s.ProtectPassword = &pw
	} else if user == "" {
		s.ProtectPassword = nil
	}
	if err := s.Check(); err != nil {
		if user == "" {
			return "", false, errs.Invalidf("protect_user", "Protection needs a user as well as a password.")
		}
		return "", false, errs.Invalidf("protect_password", "Protection needs a password as well as a user.")
	}
	blob = s.JSON()
	return blob, blob != before, nil
}

// limit is one numeric row: blank inherits, 0 is unlimited, below 0 or
// not a number is refused.
func limit[T int | float64](key, raw string, parse func(string) (T, error)) (*T, error) {
	if raw == "" {
		return nil, nil
	}
	v, err := parse(raw)
	f := float64(v)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errs.Invalidf(key, "%q is not a number.", raw)
	}
	if f < 0 {
		return nil, errs.Invalidf(key, "Use 0 or more; 0 is unlimited.")
	}
	return &v, nil
}

// saveRung is POST <card>/-/drawer/settings: the form answers itself, a
// refusal on the row it names.
func (h *handler) saveRung(kind string, own func(service.Scope) string, set func(context.Context, service.Scope, string) error) echo.HandlerFunc {
	return func(c echo.Context) error {
		cd, _ := h.cardOf(c, kind)
		form, err := c.FormParams()
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad form")
		}
		stored := own(cd.s)
		blob, changed, actErr := parseRung(stored, form)
		if actErr == nil && changed {
			actErr = set(c.Request().Context(), cd.s, blob)
		}
		if actErr == nil {
			stored = blob
		}
		msg, ok := refused(actErr)
		if actErr != nil && !ok {
			return middleware.HTTPError(actErr)
		}
		v, err := h.cascadeForm(c, cd, stored)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if actErr != nil {
			if bad, invalid := errs.IsInvalid(actErr); invalid {
				mark(&v, bad.Field, bad.Msg)
			} else {
				v.Error = msg
			}
			return respond.HTML(c, http.StatusUnprocessableEntity, comp.SettingsForm(v))
		}
		v.Note = "Nothing changed."
		if changed {
			v.Note = "Saved. Running tiles below this level redeploy with the new settings."
		}
		return respond.HTML(c, http.StatusOK, comp.SettingsForm(v))
	}
}

// mark puts a refusal on its row, or over the form when no row owns it.
func mark(v *comp.SettingsFormView, field, msg string) {
	for i := range v.Rows {
		if v.Rows[i].Key == field {
			v.Rows[i].Error = msg
			return
		}
	}
	v.Error = msg
}
