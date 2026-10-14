package deploy

import (
	"strings"
	"testing"
)

// A rotated slice credential leaves the commit, lines and params stamp alone
// but changes the rendered archive: the key must change, and show no content.
func TestFilesKeyFollowsRenderedContent(t *testing.T) {
	lines := []string{"conf/app.yml:/etc/app.yml:template"}
	a := filesKey("abc123", lines, "v1", []byte("pass: old-secret"))
	if b := filesKey("abc123", lines, "v1", []byte("pass: new-secret")); a == b {
		t.Fatal("the key ignored the rendered content")
	}
	if a != filesKey("abc123", lines, "v1", []byte("pass: old-secret")) {
		t.Error("the key is not stable for the same content")
	}
	if strings.Contains(a, "secret") {
		t.Errorf("content in the key %q", a)
	}
}
