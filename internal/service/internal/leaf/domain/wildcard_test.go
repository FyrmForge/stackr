package domain

import (
	"slices"
	"testing"
)

// One wildcard per level in use, a * covering one label only.
func TestWildcardSubjects(t *testing.T) {
	managed := []string{"example.com", "shop.co.uk"}
	hosts := []string{
		"api.web.acme.example.com", // auto name: tile.stack.org.root
		"ui.web.acme.example.com",  // same level, same wildcard
		"shop.example.com",         // typed one level
		"blog.example.com",         // shares it
		"example.com",              // apex: its own certificate
		"*.example.com",            // a wildcard host is no host
		"www.other.org",            // not managed
		"a.shop.co.uk",             // the longer managed domain
	}
	got := WildcardSubjects(managed, hosts)
	want := []Wildcard{
		{"*.example.com", "example.com"},
		{"*.shop.co.uk", "shop.co.uk"},
		{"*.web.acme.example.com", "example.com"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
