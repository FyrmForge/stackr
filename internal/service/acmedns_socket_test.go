package service

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A new panel that took the DNS socket and then failed its trial leaves a
// dead socket file; the old panel serves it again.
func TestDNSSocketServedAgainAfterTakeoverFails(t *testing.T) {
	dir := t.TempDir()
	o := &Orchestrator{cfg: Config{ProxyAdmin: "unix://" + filepath.Join(dir, "admin.sock")}, stopNodes: make(chan struct{})}
	defer close(o.stopNodes)
	old := dnsSocketCheck
	dnsSocketCheck = 20 * time.Millisecond
	defer func() { dnsSocketCheck = old }()

	o.startDNSSocket()
	sock := o.dnsSocket()
	if !dnsSocketServed(sock) {
		t.Fatal("not served at start")
	}
	// The new panel's listener, then its death without unlinking.
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if dnsSocketServed(sock) {
		t.Fatal("dead socket answers")
	}
	for end := time.Now().Add(5 * time.Second); !dnsSocketServed(sock); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the old panel never served the socket again")
		}
	}
}
