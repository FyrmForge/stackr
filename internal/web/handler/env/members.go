package env

import (
	"fmt"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/instance"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// memberRows are an instance's members as the drawer lists them.
func memberRows(vs []service.StandbyView) []instance.MemberRow {
	var out []instance.MemberRow
	for _, v := range vs {
		r := instance.MemberRow{Node: v.Node, Role: v.Role, State: v.State, Tone: "warn"}
		switch v.State {
		case "running", "streaming":
			r.Tone = "success"
		case "needs rebuild", "fenced, needs rewind":
			r.Tone = "danger"
		}
		if v.Role == "replica" && v.State == "streaming" {
			r.Lag = fmt.Sprintf("%s behind, %s", render.Size(v.LagBytes), (time.Duration(v.LagSeconds) * time.Second).String())
		}
		if v.Role == "replica" && v.SlotBytes > 0 {
			r.Slot = "slot holds " + render.Size(v.SlotBytes)
			if v.SlotCapBytes > 0 {
				r.Slot += " of " + render.Size(v.SlotCapBytes)
			}
		}
		out = append(out, r)
	}
	return out
}
