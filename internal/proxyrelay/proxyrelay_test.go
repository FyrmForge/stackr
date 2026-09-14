package proxyrelay

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startEcho returns the address of a TCP server that echoes until EOF.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c) //nolint:errcheck
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestRoundTripAndIdleExit(t *testing.T) {
	target := startEcho(t)

	// Grab a free port for the relay, then release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	done := make(chan error, 1)
	go func() { done <- Run(addr, target, 300*time.Millisecond) }()

	// The listener races the dial; retry briefly.
	var conn net.Conn
	for range 50 {
		conn, err = net.Dial("tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, err, "dial relay")

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))

	// Half-close: server's echo of nothing more, then EOF must propagate.
	conn.(*net.TCPConn).CloseWrite() //nolint:errcheck
	_, err = conn.Read(buf)
	require.ErrorIs(t, err, io.EOF, "expected EOF after half-close")
	_ = conn.Close()

	// With the connection gone, the relay must reap itself.
	select {
	case err := <-done:
		require.NoError(t, err, "Run returned")
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not idle-exit")
	}
}

func TestConnectionResetsIdleTimer(t *testing.T) {
	target := startEcho(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	done := make(chan error, 1)
	go func() { done <- Run(addr, target, 400*time.Millisecond) }()

	var conn net.Conn
	for range 50 {
		conn, err = net.Dial("tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, err)

	// Hold the connection past the idle window: the relay must stay up.
	time.Sleep(600 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("relay exited while a connection was open")
	default:
	}
	_ = conn.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not exit after last disconnect")
	}
}
