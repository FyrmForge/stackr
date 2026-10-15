package worker

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
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

// inHost carries the request's own Host to ModifyResponse.
type inHost struct{}

// registryProxy forwards the registry API (/v2/) to the front, and nothing
// else: it carries the node's client cert, and anything on the host's
// loopback reaches it (a host-network tile), so the mesh listener's other
// paths (the cert renewal, /token) must stay out of reach. Docker pulls and
// pushes here with a pass the controller minted for that one call
// (service/registry.go registryPass), which needs no token exchange.
func registryProxy(c RegistryConfig, listen string) (http.Handler, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	tc := c.TLS.Clone()
	tc.ServerName = c.Host
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Host = c.Host
			r.Out = r.Out.WithContext(context.WithValue(r.Out.Context(), inHost{}, r.In.Host))
		},
		FlushInterval: -1,
		Transport:     &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: false},
		ModifyResponse: func(res *http.Response) error {
			// The host the client used ("localhost:5000"): Docker only sends its
			// pass to the same host string.
			host, _ := res.Request.Context().Value(inHost{}).(string)
			if host == "" {
				host = listen
			}
			// A push's upload URLs name the registry host; keep them on this
			// listener, so the blobs go over the mesh too.
			if l, err := url.Parse(res.Header.Get("Location")); err == nil && l.Host == c.Host {
				l.Scheme, l.Host = "http", host
				res.Header.Set("Location", l.String())
			}
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p != "/v2" && !strings.HasPrefix(p, "/v2/") || strings.Contains(p, "..") {
			http.NotFound(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}

// serveRegistry runs the local listener until ctx ends; bound is called once
// it holds the port.
func serveRegistry(ctx context.Context, c RegistryConfig, bound func()) error {
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
	bound()
	return serve(ctx, ln, h)
}

// runRegistry serves c until ctx ends. A failure (something else bound the
// port first, say a host-network tile) is retried, and reported in Facts
// until the port is ours: the controller lists it as a cluster problem.
func (s *Server) runRegistry(ctx context.Context, c RegistryConfig) {
	for {
		err := serveRegistry(ctx, c, func() { s.regErr.Store(""); s.regUp.Store(true) })
		s.regUp.Store(false)
		if err == nil || ctx.Err() != nil {
			return
		}
		slog.Error("worker: registry proxy", "err", err)
		s.regErr.Store("registry proxy: " + err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
