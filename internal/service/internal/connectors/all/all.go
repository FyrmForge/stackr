// Package all registers the connector kinds: the only place outside a
// kind's own package that names one. A new kind is one line here.
package all

import (
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors/github"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors/namecheap"
)

// Kinds is every built-in kind, in the add dialog's order.
// namecheapAPI ("" = production) replaces Namecheap's API URL, githubURL
// and githubCA (dev.env) point GitHub at a fake and its CA.
func Kinds(namecheapAPI, githubURL, githubCA string) ([]connectors.Kind, error) {
	gh := github.Kind{}
	if githubURL != "" {
		var err error
		if gh, err = github.NewKind(githubURL, githubCA); err != nil {
			return nil, err
		}
	}
	return []connectors.Kind{gh, namecheap.Kind{Endpoint: namecheapAPI}}, nil
}
