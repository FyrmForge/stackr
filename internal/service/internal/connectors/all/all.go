// Package all registers the connector kinds: the only place outside a
// kind's own package that names one. A new kind is one line here.
package all

import (
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors/github"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors/namecheap"
)

// Kinds is every built-in kind, in the add dialog's order.
func Kinds() []connectors.Kind {
	return []connectors.Kind{github.Kind{}, namecheap.Kind{}}
}
