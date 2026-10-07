package domain

import (
	"net"
	"slices"
	"strings"
)

// Route is a host that goes to an address outside stackr (an admin-made row
// of routes). Mode is passthrough, http or https; Target is host:port.
type Route struct {
	Host, Mode, Target string
	Insecure           bool // https upstream: skip certificate verification
}

// externalRoutes turns routes into the :80 and :443 routes Build merges and,
// for pass-through hosts, the layer4 listener wrapper for the https server
// (nil when there is none). A pass-through host gets no secure row: no
// certificate of ours is issued for it, and its :80 goes to the target's own
// port 80 so the backend runs its own ACME. With TLS off nothing is secure
// and nothing passes through.
func externalRoutes(rs []Route, tlsOn bool) (plain, secure []placed, wrapper route) {
	var l4 []placed
	for _, r := range rs {
		if r.Mode == "passthrough" {
			plain = append(plain, placed{r.Host, "", serveRoute(r.Host, dial(r.Target, "80"), nil)})
			if tlsOn {
				l4 = append(l4, placed{host: r.Host, r: route{
					"match":  []route{{"tls": route{"sni": []string{r.Host}}}},
					"handle": []route{{"handler": "proxy", "upstreams": []route{{"dial": []string{r.Target}}}}},
				}})
			}
			continue
		}
		var tr route
		if r.Mode == "https" {
			tr = route{"protocol": "http", "tls": route{}}
			if r.Insecure {
				tr["tls"] = route{"insecure_skip_verify": true}
			}
		}
		serve := serveRoute(r.Host, r.Target, tr)
		if tlsOn {
			secure = append(secure, placed{r.Host, "", serve})
			plain = append(plain, placed{r.Host, "", forceRoute(r.Host, "")})
		} else {
			plain = append(plain, placed{r.Host, "", serve})
		}
	}
	if len(l4) == 0 {
		return plain, secure, nil
	}
	// layer4 takes the first match: exact hosts before wildcards.
	slices.SortStableFunc(l4, func(a, b placed) int {
		wa, wb := strings.HasPrefix(a.host, "*."), strings.HasPrefix(b.host, "*.")
		switch {
		case wa != wb && wb:
			return -1
		case wa != wb:
			return 1
		}
		return strings.Compare(a.host, b.host)
	})
	hs := make([]route, len(l4))
	for i, p := range l4 {
		hs[i] = p.r.(route)
	}
	return plain, secure, route{"wrapper": "layer4", "routes": hs}
}

func serveRoute(host, target string, transport route) route {
	rp := route{"handler": "reverse_proxy", "upstreams": []route{{"dial": target}}}
	if transport != nil {
		rp["transport"] = transport
	}
	return route{"match": match(host, "", nil), "terminal": true, "handle": []route{rp}}
}

// dial is target's host on port.
func dial(target, port string) string {
	h, _, err := net.SplitHostPort(target)
	if err != nil {
		h = target
	}
	return net.JoinHostPort(h, port)
}
