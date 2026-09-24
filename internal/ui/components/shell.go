package components

import (
	"encoding/json"

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
