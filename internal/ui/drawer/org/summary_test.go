package org

import "testing"

func TestSummaries(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{TiersSummary(TiersView{}), "No tiers"},
		{TiersSummary(TiersView{Rows: []TierRow{{Slug: "dev"}, {Slug: "staging"}, {Slug: "prod", Locked: true}}}), "dev, staging, prod · 1 locked"},
		{TiersSummary(TiersView{Rows: []TierRow{{Slug: "dev"}}}), "dev"},
		{Count(0, "share", "shares"), "No shares"},
		{Count(1, "share", "shares"), "1 share"},
		{Count(3, "share", "shares"), "3 shares"},
		{RegistrySummary(RegistryView{}), "No panel domain"},
		{RegistrySummary(RegistryView{Host: "r.x", Err: "down"}), "Not answering"},
		{RegistrySummary(RegistryView{Host: "r.x", Repos: []RepoRow{{}, {}}}), "2 repositories"},
		{RegistrySummary(RegistryView{Host: "r.x", Repo: "acme/shop/api"}), "acme/shop/api"},
	} {
		if tc.got != tc.want {
			t.Errorf("summary = %q, want %q", tc.got, tc.want)
		}
	}
}
