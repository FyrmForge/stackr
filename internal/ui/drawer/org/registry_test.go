package org

import (
	"strings"
	"testing"
	"time"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// A repository row opens its versions on click; a version shows where it came
// from and the envs that run it, and offers delete only to whoever may.
func TestRegistryVersions(t *testing.T) {
	list := html(t, Registry(RegistryView{
		Host: "registry.x.io", Org: "acme",
		Repos: []RepoRow{{Name: "acme/docker.io/library/postgres", Tags: "1 tag", Open: "/acme/-/drawer?tab=registry&repo=r"}},
	}))
	if !strings.Contains(list, `hx-get="/acme/-/drawer?tab=registry&amp;repo=r"`) {
		t.Errorf("the row does not open its versions:\n%s", list)
	}
	v := RegistryView{Repo: "acme/docker.io/library/postgres", Back: "/acme/-/drawer?tab=registry", Versions: []VersionRow{
		{Tag: "16", Size: "4.0 MB", Platforms: "amd64", Arrived: time.Date(2026, 10, 26, 7, 0, 0, 0, time.UTC), Source: "imported from postgres:16", UsedBy: []string{"shop/dev"},
			Delete: c.ConfirmView{Button: "Delete", Title: "Delete x?", Warning: "Running in shop/dev.", Action: "/acme/-/drawer/registry/delete?tag=16", Target: "#x"}},
		{Tag: "15", Size: "n/a", Source: "built"},
	}}
	got := html(t, Registry(v))
	for _, want := range []string{"imported from postgres:16", "shop/dev", "Not in use", "Back to repositories", "/acme/-/drawer/registry/delete?tag=16"} {
		if !strings.Contains(got, want) {
			t.Errorf("versions lack %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "/registry/delete") != 1 {
		t.Error("a version without a delete action got a button")
	}
}
