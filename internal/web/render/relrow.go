package render

import (
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// The words the releases window and the Releases screen share for a row.

// SourceWord is who or what cut a release, as a person reads it: names maps
// a user id to a name.
func SourceWord(by string, names map[string]string) string {
	if id, ok := strings.CutPrefix(by, "user:"); ok && names[id] != "" {
		return names[id]
	}
	switch by {
	case "push":
		return "git push"
	case "image-watch":
		return "image watch"
	}
	return by
}

// StateTone is a row state's chip tone; the rest are neutral.
var StateTone = map[string]string{
	service.StateWaiting:   "accent",
	service.StateDeploying: "warn",
	service.StateRunning:   "success",
	service.StateFailed:    "danger",
}

// ShortSHA is the seven characters a commit shows as.
func ShortSHA(sha string) string { return sha[:min(7, len(sha))] }

// ErrTail is the end of an error, where the cause is.
func ErrTail(err string) string {
	s, _ := components.ErrTail(err, 80)
	return s
}
