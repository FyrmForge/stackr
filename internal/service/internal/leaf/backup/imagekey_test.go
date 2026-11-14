package backup

import (
	"strings"
	"testing"
)

// A saved image lives under the PITR root, so Purge and DropArchive reach it,
// and is neither a base nor a WAL bundle, so window pruning leaves it alone.
func TestImageKey(t *testing.T) {
	key := ImageKey("o1", "v1", "17.11-ext-amd64-0a1b2c3d")
	if want := PITRRoot("o1", "v1") + "/image/17.11-ext-amd64-0a1b2c3d.tar.age"; key != want {
		t.Fatalf("ImageKey = %q, want %q", key, want)
	}
	if !IsPITRKey(key) {
		t.Error("an image key is not under the PITR root")
	}
	if _, _, ok := BundleLast(key); ok {
		t.Error("an image key parses as a WAL bundle")
	}
	if got := StaleWAL([]string{key}, "ffffffffffffffffffffffff"); len(got) != 0 {
		t.Errorf("StaleWAL would delete an image: %v", got)
	}
	if strings.Contains(key, "/"+BaseDir+"/") || strings.Contains(key, "/"+WALDir+"/") {
		t.Error("an image key sits in the base or wal directory")
	}
}
