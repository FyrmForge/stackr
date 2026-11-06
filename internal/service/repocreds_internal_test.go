package service

import "testing"

func TestNoRepoCreds(t *testing.T) {
	t.Parallel()
	for repo, ok := range map[string]bool{
		"acme/shop":                         true,
		"https://github.com/acme/shop":      true,
		"git@github.com:acme/shop":          true,
		"":                                  true,
		"ssh://deploy@host.example/a/b":     true,
		"ssh://deploy:pw@host.example/a/b":  false,
		"https://ghp_token@github.com/a/b":  false,
		"https://user:pw@git.example.com/a": false,
		"https://tok@git.example.com/a/b":   false,
	} {
		if err := noRepoCreds(repo); (err == nil) != ok {
			t.Errorf("noRepoCreds(%q) = %v, want ok %v", repo, err, ok)
		}
	}
}
