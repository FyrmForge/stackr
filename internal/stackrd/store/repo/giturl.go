package repo

import (
	"net/url"
	"strings"
)

// gitHosts is the allow-list for a tile's git_url. GitHub only today; add a
// host here when a connector for it exists.
var gitHosts = []string{"github.com"}

// ValidGitURL accepts https://<host>/owner/repo[.git] and
// git@<host>:owner/repo[.git] for hosts in gitHosts. Everything else is
// refused, which is the point: git happily clones file:// and bare local
// paths, so an unchecked git_url would let a build pull the server's own data
// directory. A leading dash is refused too, it would read as a git flag.
func ValidGitURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" || strings.HasPrefix(u, "-") {
		return false
	}
	for _, h := range gitHosts {
		if p, ok := strings.CutPrefix(u, "git@"+h+":"); ok {
			return validRepoPath(p)
		}
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.User != nil {
		return false
	}
	if !allowedHost(p.Hostname()) {
		return false
	}
	return validRepoPath(strings.TrimPrefix(p.Path, "/"))
}

func allowedHost(h string) bool {
	for _, a := range gitHosts {
		if strings.EqualFold(h, a) {
			return true
		}
	}
	return false
}

// validRepoPath wants exactly owner/repo, no traversal, no empty halves.
func validRepoPath(p string) bool {
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return false
	}
	for _, s := range parts {
		if s == "" || strings.HasPrefix(s, "-") || s == "." || s == ".." {
			return false
		}
	}
	return true
}
