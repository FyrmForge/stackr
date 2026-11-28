package deploy

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestShareDenied(t *testing.T) {
	r := resolved{shareVols: map[string]store.Share{"stackr-share-x": {Slug: "media", Source: "nas.lan:/export"}}}
	raw := "mount: permission denied"
	in := docker.MountDeniedError{Volume: "stackr-share-x", Raw: raw}
	var log bytes.Buffer
	err := r.shareDenied(in, &log)
	msg := err.Error()
	if !strings.HasPrefix(msg, "The NAS refused access to share media (nas.lan:/export)") || !strings.HasSuffix(msg, "\n"+raw) {
		t.Fatalf("message = %q", msg)
	}
	if !errors.Is(err, in) || !strings.Contains(log.String(), raw) {
		t.Fatalf("original lost or not logged: %v %q", err, log.String())
	}
	other := docker.MountDeniedError{Volume: "plain", Raw: raw}
	if got := r.shareDenied(other, &log); got != error(other) {
		t.Fatalf("non-share volume changed: %v", got)
	}
	plain := errors.New("port is already allocated")
	if got := r.shareDenied(plain, &log); got != plain {
		t.Fatalf("unrelated error changed: %v", got)
	}
	// Any other error carrying the mount options is masked, and stays wrapped.
	leaky := &net.OpError{Op: "mount", Err: errors.New("failed: data: username=bob,password=hunter2,addr=1.2.3.4")}
	got := r.shareDenied(leaky, &log)
	if strings.Contains(got.Error(), "hunter2") || strings.Contains(got.Error(), "bob") {
		t.Fatalf("leaked: %q", got)
	}
	var oe *net.OpError
	if !errors.As(got, &oe) {
		t.Fatalf("original lost: %v", got)
	}
	if got := (resolved{}).shareDenied(leaky, &log); got != error(leaky) {
		t.Fatalf("no share volume, error changed: %v", got)
	}
}
