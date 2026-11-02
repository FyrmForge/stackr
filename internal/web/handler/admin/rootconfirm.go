package admin

import (
	"context"
	"fmt"
	"strings"

	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

// bareRoot is a root_domain value as a host: no wildcard, no case.
func bareRoot(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "*.")
}

// rootMoveConfirm is the question a changed root_domain asks before the
// save renames hosts, nil when the save moves none (or the old root has no
// instance row to count). vals is the posted knobs.
func (h *handler) rootMoveConfirm(ctx context.Context, action string, vals map[string]string) *comp.ConfirmView {
	next, ok := vals["root_domain"]
	if !ok {
		return nil
	}
	ss, err := h.orch.ServerSettings(ctx)
	if err != nil {
		return nil
	}
	var old string
	for _, s := range ss {
		if s.Key == "root_domain" {
			old = bareRoot(s.Effective)
		}
	}
	if old == "" || bareRoot(next) == old {
		return nil
	}
	all, err := h.orch.AllDomainResources(ctx)
	if err != nil {
		return nil
	}
	for _, r := range all {
		if r.Level != "instance" || r.Host != old {
			continue
		}
		imp, err := h.orch.DomainResourceImpact(ctx, r.ID)
		if err != nil || (imp.Hosts == 0 && imp.Resources < 2) {
			return nil
		}
		to := bareRoot(next)
		if to == "" {
			to = "no root domain"
		}
		cv := &comp.ConfirmView{
			Button: "Move root domain", Title: "Move the root domain to " + to + "?",
			Warning: fmt.Sprintf("Renames %d domains and %d app hosts under %s, and issues %d certificates. Each old host redirects to its new one.", imp.Resources, imp.Hosts, old, imp.Certs),
			Action:  action, Target: "#admin-domains", Vals: `{"confirm_root":"1"}`, Open: true,
		}
		if moves, err := h.orch.DomainResourceMoves(ctx, r.ID, bareRoot(next)); err == nil && len(moves) > 0 {
			more := max(len(moves)-8, 0)
			cv.Body = ui.HostMoves(moves[:len(moves)-more], more)
		}
		return cv
	}
	return nil
}
