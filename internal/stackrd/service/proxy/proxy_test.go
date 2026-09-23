package proxy

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
)

func TestParseTrustedList(t *testing.T) {
	// Commas and newlines, because the panel form posts one and the
	// installer's env var the other, and they used to be parsed by two
	// different loops with two different policies.
	got, err := ParseTrustedList("10.0.0.0/8, 192.168.1.100\n\n  172.16.0.0/12  ")
	if err != nil {
		t.Fatalf("valid list: %v", err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.100/32", "172.16.0.0/12"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// One bad entry refuses the list. Skipping it leaves a trusted proxy that
	// is not trusted, which reads as every client IP being the proxy's.
	if _, err := ParseTrustedList("10.0.0.0/8, nope"); err == nil {
		t.Fatal("bad CIDR accepted")
	} else {
		var inv svcerr.Invalid
		if !errors.As(err, &inv) {
			t.Errorf("want svcerr.Invalid so the handler can answer 400, got %T", err)
		}
	}
}

// EnsureTraefik rewrites the static config and restarts a container. Two saves
// used to race two goroutines through that. One runs at a time, and however
// many callers arrive while one is in flight collapse into a single follow-up
// — but at least one follow-up, or a save made mid-run is never applied.
func TestEnsureTraefikSerializesAndCoalesces(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	concurrent := false

	s := &Service{}
	s.ensure = func(context.Context) error {
		mu.Lock()
		runs++
		if runs > 0 && len(entered) > 0 {
			// Something else got in while we were running.
			concurrent = true
		}
		n := runs
		mu.Unlock()
		if n == 1 {
			entered <- struct{}{}
			<-release // hold the first run open while the others pile up
			<-entered
		}
		return nil
	}

	s.EnsureTraefik()
	<-entered
	entered <- struct{}{} // put it back: the first run is still in flight
	for range 5 {
		s.EnsureTraefik() // all arrive mid-run
	}
	close(release)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n, bad := runs, concurrent
		mu.Unlock()
		if bad {
			t.Fatal("two runs overlapped; the rewrite-and-restart is not serialized")
		}
		if n == 2 {
			return // one in flight plus exactly one coalesced follow-up
		}
		if n > 2 {
			t.Fatalf("five mid-run callers produced %d follow-ups, want 1", n-1)
		}
		select {
		case <-deadline:
			t.Fatalf("only %d run(s); a caller arriving mid-run left no follow-up", n)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// The boot path returns its error but must still share the slot: it runs in a
// goroutine while the HTTP server is already accepting, so an operator saving
// proxy settings during the image pull lands mid-run. Taking the slot without
// checking ran two reconfigures at once; clearing the queue on the way out
// dropped that save silently — the panel said "saved" and Traefik never saw it.
func TestEnsureTraefikNowKeepsMidRunSave(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	inFirst := make(chan struct{})
	release := make(chan struct{})

	s := &Service{}
	s.ensure = func(context.Context) error {
		mu.Lock()
		runs++
		n := runs
		mu.Unlock()
		if n == 1 {
			close(inFirst)
			<-release
		}
		return errors.New("boot pull failed")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.EnsureTraefikNow(context.Background()) }()
	<-inFirst

	s.EnsureTraefik() // the operator's save, mid-boot
	close(release)

	if err := <-errCh; err == nil {
		t.Fatal("boot must get its error back, not a log line")
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := runs
		mu.Unlock()
		if n >= 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("a save made during boot was dropped: no follow-up run")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
