package runtime

import (
	"fmt"
	"strings"
)

// ParseDep decodes one depends_on line: "slug" or "slug:condition".
// Bare slug means started.
//
// It lives beside ParseDevice and ParseFileMount because it is the same kind
// of thing — the grammar of one line of a tile column — and because both the
// config engine and the tile service validate it, and the service cannot
// import the config engine without closing a cycle.
func ParseDep(line string) (slug, cond string, err error) {
	slug, cond = line, "started"
	if i := strings.IndexByte(line, ':'); i >= 0 {
		slug, cond = line[:i], line[i+1:]
	}
	if slug == "" {
		return "", "", fmt.Errorf("depends_on %q: empty tile slug", line)
	}
	switch cond {
	case "started", "healthy", "completed":
		return slug, cond, nil
	}
	return "", "", fmt.Errorf("depends_on %q: condition must be started, healthy or completed", line)
}
