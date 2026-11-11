// Package livecheck holds the one table test that every live region of the
// panel's drawers and pages carries its poller.
package livecheck

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/FyrmForge/stackr/internal/ui/connectors"
	"github.com/FyrmForge/stackr/internal/ui/dns"
	envui "github.com/FyrmForge/stackr/internal/ui/drawer/env"
	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
	sliceui "github.com/FyrmForge/stackr/internal/ui/drawer/slice"
	volumeui "github.com/FyrmForge/stackr/internal/ui/drawer/volume"
	setupui "github.com/FyrmForge/stackr/internal/ui/pages/setup"
	"github.com/FyrmForge/stackr/internal/ui/placement"
)

func TestLiveRegionsPoll(t *testing.T) {
	for name, tc := range map[string]struct {
		c    templ.Component
		want string
	}{
		"placement moving": {placement.Placement(placement.View{Scope: "env", RunsOn: true, Live: "/p"}), `hx-select="#placement-moving"`},
		"placement offers": {placement.Placement(placement.View{Scope: "org", RunsOn: true, Live: "/p"}), `hx-select="#placement-offers"`},
		"org members":      {orgui.Members(orgui.MembersView{Base: "/o"}), `hx-select="#org-members"`},
		"org keys":         {orgui.Keys(orgui.KeysView{Base: "/o"}), `hx-select="#org-keys"`},
		"slice overview":   {sliceui.Overview(sliceui.View{Base: "/s"}), `hx-select="#tab-overview"`},
		"connectors list":  {connectors.List(connectors.ListView{Live: "/c"}), `hx-select="#connectors-list"`},
		"dns records":      {dns.Page(dns.PageView{Live: "/d"}), `hx-select="#dns-page"`},
		"env sync":         {envui.Sync(envui.SyncView{Live: "/e"}), `hx-select="#tab-sync"`},
		"volume history":   {volumeui.History(volumeui.BackupsView{ID: "v", Base: "/v"}), `every 3s`},
	} {
		var b strings.Builder
		if err := tc.c.Render(context.Background(), &b); err != nil {
			t.Fatal(name, err)
		}
		if !strings.Contains(b.String(), tc.want) {
			t.Errorf("%s: no %s in %s", name, tc.want, b.String())
		}
	}
	var b strings.Builder
	_ = orgui.Keys(orgui.KeysView{Base: "/o", Minted: "tok"}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "every 3s") {
		t.Error("keys poll over a freshly minted token")
	}
	b.Reset()
	_ = setupui.ServerSummary(setupui.ServerSummaryView{ServerStep: setupui.ServerStep{Base: "/-/setup/server"}}).Render(context.Background(), &b)
	if !strings.Contains(b.String(), `hx-select="#setup-summary"`) {
		t.Error("setup summary is not live")
	}
}
