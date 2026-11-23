package image

import "github.com/FyrmForge/stackr/internal/service/internal/store"

// Status says what image watch knows of i, in words: "unknown" whenever it
// cannot tell (never read, the last read failed, nothing runs yet), never
// blank and never "up to date" on a guess.
func Status(i store.Image) string {
	switch {
	case i.LastDigest == "" || i.LastError != "" || i.Digest == "":
		return "unknown"
	case i.Newer():
		return "newer image available"
	}
	return "up to date"
}
