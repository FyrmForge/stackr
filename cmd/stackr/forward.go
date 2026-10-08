package main

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
)

func (a *app) forward() *cobra.Command {
	var spec, address, container string
	c := leaf(
		"forward [tile]",
		"tile.forward",
		"Tunnel a local port to a tile's port",
		upTo(1),
		a.at(atTile, func(_ *cobra.Command, p string, _ []string) error {
			if a.json {
				return usage("forward has no machine-readable output; drop --json")
			}
			local, remote, err := parsePorts(spec)
			if err != nil {
				return usage("%v", err)
			}
			q := url.Values{"container": {container}}
			if remote > 0 {
				q.Set("port", strconv.Itoa(remote))
			}
			// The probe connection: a stopped tile, a bad port or a missing
			// scope fails here, and the reply names the port the server chose.
			probe, res, err := a.dial(p+"/forward", q)
			if err != nil {
				return err
			}
			_ = probe.Close(websocket.StatusNormalClosure, "")
			if remote, err = strconv.Atoi(res.Header.Get("X-Stackr-Port")); err != nil {
				return fmt.Errorf("the server did not say which port it reached")
			}
			q.Set("port", strconv.Itoa(remote))
			if local == 0 {
				local = remote
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(local)))
			if err != nil {
				return err
			}
			// Cancelling the context alone does not unblock Accept.
			go func() { <-a.ctx.Done(); _ = ln.Close() }()
			defer func() { _ = ln.Close() }()
			_, _ = fmt.Fprintf(a.out, "Forwarding %s -> %s:%d (Ctrl-C to stop)\n", ln.Addr(), tileName(p), remote)
			for {
				conn, err := ln.Accept()
				if err != nil {
					return nil // listener closed: a clean stop
				}
				go a.tunnel(conn, p+"/forward", q)
			}
		}),
	)
	c.Flags().StringVar(&spec, "port", "", "[local:]remote port (default: the tile's own)")
	c.Flags().StringVar(&address, "address", "127.0.0.1", "local address to bind")
	c.Flags().StringVar(&container, "container", "", "the replica (default: a running one)")
	return scoped(c, true)
}

// tunnel pipes one local connection through its own websocket.
func (a *app) tunnel(conn net.Conn, path string, q url.Values) {
	defer func() { _ = conn.Close() }()
	ws, _, err := a.dial(path, q)
	if err != nil {
		_, _ = fmt.Fprintf(a.errw, "forward: %v\n", err)
		return
	}
	defer func() { _ = ws.CloseNow() }()
	nc := websocket.NetConn(a.ctx, ws, websocket.MessageBinary)
	// Wait on the reply direction, not on whichever ends first: psql, curl
	// and `nc -N` stop sending before the answer arrives.
	go func() {
		if _, err := io.Copy(nc, conn); err != nil {
			_ = ws.CloseNow() // the local side broke: nothing more to carry
			return
		}
		// the local side finished sending: the tile gets its EOF, the reply still comes
		_ = ws.Write(a.ctx, websocket.MessageText, []byte(`{"type":"eof"}`))
	}()
	_, _ = io.Copy(conn, nc)
}

// tileName is the tile's slug from an API path ending /tiles/<slug>.
func tileName(p string) string { return p[strings.LastIndex(p, "/")+1:] }

// parsePorts reads kubectl's "[local:]remote". A bare number is the local
// port only; 0 means the tile's own.
func parsePorts(s string) (local, remote int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	l, r, split := strings.Cut(s, ":")
	if local, err = parsePort(l); err != nil || !split {
		return local, 0, err
	}
	remote, err = parsePort(r)
	return local, remote, err
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return n, nil
}
