// Package proxyrelay is the entire brain of the stackr-proxyrelay container:
// a TCP pipe with an idle timeout. stackr starts one per forwarded (tile,
// port) so the thing bridging the stackr network and an environment network
// is this, not the process holding the docker socket.
package proxyrelay

import (
	"io"
	"net"
	"sync"
	"time"
)

// Run listens on addr and pipes every accepted connection to target. It
// returns nil once no connection has been open for idle, the container's
// self-reap, or an error if the listener cannot be created. target is dialled
// per connection (a DNS alias, so a redeployed tile re-resolves).
func Run(addr, target string, idle time.Duration) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	var (
		mu     sync.Mutex
		active int
		last   = time.Now()
	)

	// Idle reaper: closing the listener is the exit signal.
	go func() {
		tick := time.NewTicker(idle / 4)
		defer tick.Stop()
		for range tick.C {
			mu.Lock()
			expired := active == 0 && time.Since(last) >= idle
			mu.Unlock()
			if expired {
				_ = ln.Close()
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for {
		c, err := ln.Accept()
		if err != nil {
			// Listener closed by the reaper. Open connections cannot exist
			// (active was 0) but a just-accepted one might; wait it out.
			wg.Wait()
			return nil
		}
		mu.Lock()
		active++
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer func() {
				_ = c.Close()
				mu.Lock()
				active--
				last = time.Now()
				mu.Unlock()
				wg.Done()
			}()
			t, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				return
			}
			pipe(c, t)
			_ = t.Close()
		}()
	}
}

// pipe copies both directions, half-closing the write side of each peer when
// the other's read side ends, and returns when both directions are done, so
// a client that sends EOF still receives the server's full response.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(a, b); closeWrite(a) }() //nolint:errcheck
	go func() { defer wg.Done(); io.Copy(b, a); closeWrite(b) }() //nolint:errcheck
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.CloseWrite()
	}
}
