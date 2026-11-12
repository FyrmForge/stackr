package connpanel

import (
	"testing"
	"time"
)

// An install answer is served for installTTL and asked of the provider
// again after.
func TestCachedInstallTTL(t *testing.T) {
	installMu.Lock()
	installCache["o/c"] = installAnswer{repos: 3, at: time.Now()}
	installCache["o/old"] = installAnswer{repos: 3, at: time.Now().Add(-2 * installTTL)}
	installMu.Unlock()
	if a, ok := cachedInstall("o/c"); !ok || a.repos != 3 {
		t.Errorf("fresh answer not served: %v %v", a, ok)
	}
	if _, ok := cachedInstall("o/old"); ok {
		t.Error("stale answer served")
	}
	if _, ok := cachedInstall("o/none"); ok {
		t.Error("missing answer served")
	}
}
