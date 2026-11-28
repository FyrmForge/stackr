package deploy

import (
	"bytes"
	"errors"
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
}
