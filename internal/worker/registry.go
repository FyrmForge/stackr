package worker

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
)

// RegistryListen is where Docker on the node finds the controller's registry
// with no daemon config (Docker trusts 127.0.0.0/8 as an insecure registry).
const RegistryListen = "127.0.0.1:5000"

// RegistryConfig makes the worker serve the controller's registry locally.
// URL is the controller panel's address on its transit IP, Host the name the
// registry front answers to (registry.<panel domain>, also the TLS server
// name). TLS carries the node's client cert, so the front applies the node's
// facts (see service/registry.go registryFacts). Empty URL = off.
type RegistryConfig struct {
	URL, Host string
	TLS       *tls.Config
	Listen    string // "" = RegistryListen
}

var realmRe = regexp.MustCompile(`realm="[^"]*"`)

// registryProxy forwards everything to the front. The token realm in the
// 401 challenge is rewritten to this listener, so Docker's token fetch comes
// back through the proxy (and its client cert) instead of straight to the
// controller, where it has no cert.
func registryProxy(c RegistryConfig, listen string) (http.Handler, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	tc := c.TLS.Clone()
	tc.ServerName = c.Host
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Host = c.Host
		},
		FlushInterval: -1,
		Transport:     &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: false},
		ModifyResponse: func(res *http.Response) error {
			if h := res.Header.Get("WWW-Authenticate"); h != "" {
				res.Header.Set("WWW-Authenticate", realmRe.ReplaceAllLiteralString(h, `realm="http://`+listen+`/token"`))
			}
			// A push's upload URLs name the registry host; keep them on this
			// listener, so the blobs go over the mesh too.
			if l, err := url.Parse(res.Header.Get("Location")); err == nil && l.Host == c.Host {
				l.Scheme, l.Host = "http", listen
				res.Header.Set("Location", l.String())
			}
			return nil
		},
	}, nil
}

// serveRegistry runs the local listener until ctx ends.
func serveRegistry(ctx context.Context, c RegistryConfig) error {
	listen := c.Listen
	if listen == "" {
		listen = RegistryListen
	}
	h, err := registryProxy(c, listen)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	return serve(ctx, ln, h)
}
