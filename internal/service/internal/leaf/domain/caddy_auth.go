package domain

import (
	"net"
	"net/http"
	"net/url"
	"slices"
)

// defaultCopyHeaders are what an auth provider hands back when the domain
// names none (the Authelia and Authentik convention).
var defaultCopyHeaders = []string{"Remote-User", "Remote-Groups", "Remote-Name", "Remote-Email"}

// authHandlers are the handlers that gate a request before it reaches the
// tile: basic auth from the domain's extras, else the settings cascade's
// protection, then forward auth (so the provider never sees the credentials
// of a visitor who fails basic auth). Empty when the route is open.
func authHandlers(e Extras, t TileRoute, expand Expand) []route {
	var hs []route
	auth := e.BasicAuth
	if auth == nil {
		auth = t.Protect
	}
	if auth != nil {
		user, hash := credentials(*auth, expand)
		hs = append(hs, route{
			"handler": "authentication",
			"providers": route{"http_basic": route{
				"hash":     route{"algorithm": "bcrypt"},
				"accounts": []route{{"username": user, "password": hash}},
			}},
		})
	}
	return append(hs, forwardAuth(e.ForwardAuth)...)
}

// forwardAuth is what the Caddyfile forward_auth directive expands to: a GET
// with no body to the provider, then on 2xx the CopyHeaders ride on the
// request and it continues; anything else is the provider's own answer. The
// URL and names were checked by checkProxyStrings, which domainRoute runs
// first so a bad one leaves the route out; a bad one here yields nothing
// (open), so that check must stay.
func forwardAuth(fa *ForwardAuth) []route {
	if fa == nil {
		return nil
	}
	u, err := url.Parse(fa.URL)
	if err != nil || u.Host == "" {
		return nil
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	names := fa.CopyHeaders
	if len(names) == 0 {
		names = defaultCopyHeaders
	}
	// Like the directive: delete each copy header unconditionally (a client
	// must not smuggle its own), then set it only when the provider sent one
	// (Caddy leaves an unknown placeholder as literal text).
	good := []route{{"handle": []route{{"handler": "vars"}}}}
	var seen []string
	for _, n := range names {
		n = http.CanonicalHeaderKey(n)
		if slices.Contains(seen, n) {
			continue
		}
		seen = append(seen, n)
		ph := "{http.reverse_proxy.header." + n + "}"
		good = append(good,
			route{"handle": []route{{"handler": "headers", "request": route{"delete": []string{n}}}}},
			route{
				"match": []route{{"not": []route{{"vars": route{ph: []string{""}}}}}},
				"handle": []route{{
					"handler": "headers", "request": route{"set": route{n: []string{ph}}},
				}},
			})
	}
	// The URI always carries a query, even an empty one: Caddy keeps the
	// client's own query on a rewrite that names none.
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	rp := route{
		"handler":   "reverse_proxy",
		"upstreams": []route{{"dial": net.JoinHostPort(u.Hostname(), port)}},
		// The rewrite to GET drops the request body; the URI is always set
		// so the client's own path and query never reach the provider.
		"rewrite": route{"method": "GET", "uri": path + "?" + u.RawQuery},
		"headers": route{"request": route{
			"delete": []string{"X-Original-Url", "X-Original-Method"},
			"set": route{
				"Host":               []string{"{http.reverse_proxy.upstream.hostport}"},
				"X-Forwarded-Method": []string{"{http.request.method}"},
				"X-Forwarded-Uri":    []string{"{http.request.uri}"},
				"X-Forwarded-Host":   []string{"{http.request.host}"},
			},
		}},
		"handle_response": []route{
			{"match": route{"status_code": []int{2}}, "routes": good},
			{"routes": []route{{"handle": []route{{"handler": "copy_response"}}}}},
		},
	}
	if u.Scheme == "https" {
		rp["transport"] = route{"protocol": "http", "tls": route{}}
	}
	return []route{rp}
}
