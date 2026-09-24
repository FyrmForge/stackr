package components

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/a-h/templ"
)

// Shell is what the layout needs: built once per request by render.Page
// from the URL and the principal, never by a page.
type Shell struct {
	Title  string
	CSRF   string // sent on every htmx request as X-CSRF-Token
	User   string // signed-in email; "" = anonymous
	Crumbs []Link // org / stack / env / tile, from the URL
	Nav    []Link // top-level sections; Active marks the one the URL is in
	Admin  bool   // the nav opens the admin drawer
	Flash  Flash
	// EnvColor is the URL's env hue (one of EnvColors): the top bar's band
	// and the env crumb's dot. "" = no env, or no hue picked.
	EnvColor string
	// Actions is the top bar's right side (a canvas's create buttons);
	// nil = none. It shows only where the bar does (Crumbs set).
	Actions templ.Component
	// Drawer is the drawer body a fresh load of ?drawer=&tab= opens with;
	// nil = closed. Tab is the ?tab= it shows.
	Drawer templ.Component
	Tab    string
}

// Link is one navigation target.
type Link struct {
	Label  string
	Href   string
	Active bool
}

// Flash is a one-shot message. Kind is success, error, warning or info.
type Flash struct {
	Message string
	Kind    string
}

// htmxConfig replaces the scaffold's inline script: 4xx form answers swap,
// 5xx and the rest raise htmx:responseError for <flash-toast>.
const htmxConfig = `{"responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},{"code":"400","swap":true,"error":false},{"code":"422","swap":true,"error":false},{"code":"429","swap":true,"error":false},{"code":"[45]..","swap":false,"error":true}]}`

func csrfHeaders(token string) string {
	b, _ := json.Marshal(map[string]string{"X-CSRF-Token": token})
	return string(b)
}

// up is the top bar's ← target: the last crumb's parent; "" at the org,
// where v0's bar had none.
func up(crumbs []Link) string {
	if len(crumbs) < 2 {
		return ""
	}
	return crumbs[len(crumbs)-2].Href
}

// Initials picks up to two leading letters for an avatar badge: one per
// word, plus a capital inside a word ("FyrmForge" is FF). v0 avatar.templ.
func Initials(name string) string {
	var out []rune
	for _, f := range strings.Fields(name) {
		for i, r := range f {
			if i != 0 && !unicode.IsUpper(r) {
				continue
			}
			out = append(out, r)
			if len(out) == 2 {
				return strings.ToUpper(string(out))
			}
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return strings.ToUpper(string(out))
}

// nav is the Nav entry for href; ok false when the viewer has none.
func (s Shell) nav(href string) (Link, bool) {
	for _, l := range s.Nav {
		if l.Href == href {
			return l, true
		}
	}
	return Link{}, false
}
