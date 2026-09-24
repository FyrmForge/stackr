package tile

import (
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/tile"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// when is how the drawer spells a moment.
func when(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("Jan 2 15:04")
}

func replicas(s service.TileStatus) []ui.ReplicaView {
	out := make([]ui.ReplicaView, 0, len(s.Replicas))
	for _, r := range s.Replicas {
		out = append(out, ui.ReplicaView{ID: r.ID, Name: r.Name, State: r.State, Health: r.Health})
	}
	return out
}

func statusView(env string, s service.TileStatus) ui.StatusView {
	v := ui.StatusView{Word: s.Word, Replicas: replicas(s), NextRun: when(s.NextRun), Stopped: s.Word == "stopped"}
	if s.LastJob != nil {
		j := render.JobView(env, *s.LastJob)
		v.Job = &j
	}
	if s.LastRun != nil {
		v.LastRun = s.LastRun.Status + " · " + when(&s.LastRun.CreatedAt)
	}
	return v
}

func logsView(c echo.Context, base string, s service.TileStatus) ui.LogsView {
	v := ui.LogsView{Replicas: replicas(s), Container: c.QueryParam("container"), Run: c.QueryParam("run")}
	if v.Container == "" && len(v.Replicas) > 0 {
		v.Container = v.Replicas[0].ID
	}
	url := base + "/logs/stream?container=" + v.Container
	if v.Run != "" {
		url = base + "/logs/stream?run=" + v.Run
	}
	v.Pane = comp.LogPaneView{StreamURL: url, Level: c.QueryParam("level"), Search: c.QueryParam("q")}
	return v
}

func domainsView(ds []service.Domain, admin bool) ui.DomainsView {
	v := ui.DomainsView{Admin: admin}
	for _, d := range ds {
		v.Rows = append(v.Rows, ui.DomainRow{ID: d.ID, Host: d.Host, Path: d.Path, Port: strconv.Itoa(d.ContainerPort),
			HTTPS: d.HTTPS, Auto: d.Auto, Raw: d.RawCaddy})
	}
	return v
}

// settingsView is the tile rung of the two tile-scope knobs. ponytail: no
// verb answers the cascade at tile level, so "applies" is the tile's own
// value or the server default.
func settingsView(base string, t service.Tile, errors map[string]string) comp.SettingsFormView {
	row := func(key, desc, typ, val string) comp.SettingRowView {
		r := comp.SettingRowView{Key: key, Desc: desc, Type: typ, Value: val, Effective: val, DecidedBy: "tile", Error: errors[key]}
		if val == "" {
			r.Effective, r.DecidedBy = "no limit", "default"
		}
		return r
	}
	cpu, mem := "", ""
	if t.CPULimit != 0 {
		cpu = strconv.FormatFloat(t.CPULimit, 'f', -1, 64)
	}
	if t.MemLimitMB != 0 {
		mem = strconv.Itoa(t.MemLimitMB)
	}
	return comp.SettingsFormView{ID: "tile-settings", Action: base + "/settings", Scope: "Tile", Rows: []comp.SettingRowView{
		row("cpu_limit", "CPUs each replica may use.", "float", cpu),
		row("mem_limit_mb", "Memory each replica may use, in MB.", "int", mem),
	}}
}

func jobsView(js []service.Job) ui.JobsView {
	var v ui.JobsView
	for _, j := range js {
		v.Rows = append(v.Rows, ui.JobRow{Kind: j.Kind, State: j.State, When: when(&j.CreatedAt), Error: j.Error})
	}
	return v
}

func imageView(t service.Tile, i service.Image) ui.ImageView {
	return ui.ImageView{Ref: t.ImageRef, Digest: i.Digest, LastDigest: i.LastDigest, LastTag: i.LastTag,
		Checked: when(i.CheckedAt), LastError: i.LastError,
		NewVersion:   i.Newer(),
		UpdatePolicy: t.UpdatePolicy, TagPolicy: t.TagPolicy}
}

func runsView(base string, t service.Tile, rs []service.Run) ui.RunsView {
	v := ui.RunsView{Cron: t.Kind == "cron", Paused: t.Paused}
	for _, r := range rs {
		row := ui.RunRow{ID: r.ID, Status: r.Status, Trigger: r.Trigger, Started: when(r.StartedAt),
			Live: r.Status == "queued" || r.Status == "running"}
		if r.StartedAt != nil && r.FinishedAt != nil {
			row.Took = r.FinishedAt.Sub(*r.StartedAt).Round(time.Second).String()
		}
		if r.ExitCode != nil {
			row.Exit = strconv.Itoa(*r.ExitCode)
		}
		v.Rows = append(v.Rows, row)
		if row.Live {
			v.Poll = base + "?tab=runs"
		}
	}
	return v
}

func backupsView(env string, vs []service.Volume) ui.BackupsView {
	var v ui.BackupsView
	for _, x := range vs {
		state := "in use"
		if x.OrphanedAt != nil {
			state = "orphaned"
		}
		v.Rows = append(v.Rows, ui.VolumeRow{ID: x.ID, Name: x.Name, State: state, Drawer: env + "/-/volumes/" + x.ID + "?tab=backups"})
	}
	return v
}
