package tile

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
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

// runKind: a cron or function keeps no container up; its runs are its life.
func runKind(kind string) bool {
	return kind == "cron" || kind == "function"
}

// pulls mirrors the tile leaf's Pulls: the artifact is a registry image the
// row names, so the watcher has something to say about it.
func pulls(t service.Tile) bool {
	return t.Kind == "image" || (runKind(t.Kind) && t.GitURL == "")
}

// source is the header subtitle (v0 appSubtitle): the image a tile runs,
// or the repo it builds.
func source(t service.Tile) string {
	if t.GitURL == "" {
		return t.ImageRef
	}
	s := strings.TrimPrefix(t.GitURL, "https://")
	return strings.TrimPrefix(s, "git@")
}

func replicas(s service.TileStatus) []ui.ReplicaView {
	out := make([]ui.ReplicaView, 0, len(s.Replicas))
	for _, r := range s.Replicas {
		out = append(out, ui.ReplicaView{
			ID:     r.ID,
			Name:   r.Name,
			State:  r.State,
			Health: r.Health,
		})
	}
	return out
}

func statusView(env string, t service.Tile, s service.TileStatus, ds []service.Domain) ui.StatusView {
	v := ui.StatusView{
		Word:     s.Word,
		Replicas: replicas(s),
		URLs:     domainRows(ds),
		Ports:    strings.Fields(t.PublishedPorts),
	}
	if t.ContainerPort != 0 {
		v.Port = strconv.Itoa(t.ContainerPort)
	}
	if s.LastJob != nil {
		j := render.JobView(env, *s.LastJob)
		v.Job = &j
		v.JobWhen = when(&s.LastJob.CreatedAt)
	}
	return v
}

// logsView follows a replica, or a run: the one asked for, else a run
// kind's latest.
func logsView(c echo.Context, base string, t service.Tile, s service.TileStatus, lastRun string) ui.LogsView {
	v := ui.LogsView{
		Replicas:  replicas(s),
		Container: c.QueryParam("container"),
		Run:       c.QueryParam("run"),
		Runs:      runKind(t.Kind),
	}
	if v.Runs && v.Run == "" {
		v.Run = lastRun
	}
	if !v.Runs && v.Container == "" && len(v.Replicas) > 0 {
		v.Container = v.Replicas[0].ID
	}
	url := base + "/logs/stream?container=" + v.Container
	if v.Run != "" {
		url = base + "/logs/stream?run=" + v.Run
	}
	v.Pane = comp.LogPaneView{StreamURL: url, Level: c.QueryParam("level"), Search: c.QueryParam("q")}
	return v
}

func domainRows(ds []service.Domain) []ui.DomainRow {
	var out []ui.DomainRow
	for _, d := range ds {
		out = append(out, ui.DomainRow{
			ID:       d.ID,
			Host:     d.Host,
			Path:     d.Path,
			Port:     strconv.Itoa(d.ContainerPort),
			HTTPS:    d.HTTPS,
			Auto:     d.Auto,
			Redirect: d.RedirectTo,
			Raw:      d.RawCaddy,
		})
	}
	return out
}

// envView is the tile's env as rows and as the editor's lines.
func envView(blob string) ui.EnvView {
	m := envMap(blob)
	var v ui.EnvView
	var lines []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		val := m[k]
		v.Rows = append(v.Rows, ui.EnvRow{Name: k, Value: val, Ref: strings.Contains(val, "${{")})
		if strings.Contains(val, "\n") {
			v.Kept = append(v.Kept, k)
			continue
		}
		lines = append(lines, k+"="+val)
	}
	v.Text = strings.Join(lines, "\n")
	return v
}

// envMap reads a tile's JSON map column; Validate keeps it a map of
// strings, so a bad blob reads as empty.
func envMap(blob string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(blob), &m)
	return m
}

func encode(m map[string]string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(m)
	return strings.TrimSpace(b.String())
}

// withLines is blob after the editor saved text (KEY=VALUE per line): the
// lines replace the map, except a value on more than one line, which no
// line could hold, stays. An unchanged map keeps the stored text, so an
// untouched save is no change at all.
func withLines(key, blob, text string) (string, error) {
	next := map[string]string{}
	for i, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		k, val, ok := strings.Cut(l, "=")
		if !ok {
			return "", errs.Invalidf(key, "line %d: want KEY=VALUE", i+1)
		}
		next[strings.TrimSpace(k)] = val
	}
	cur := envMap(blob)
	for k, val := range cur {
		if _, set := next[k]; !set && strings.Contains(val, "\n") {
			next[k] = val
		}
	}
	if maps.Equal(cur, next) {
		return blob, nil
	}
	return encode(next), nil
}

// without is blob with name dropped.
func without(blob, name string) string {
	m := envMap(blob)
	delete(m, name)
	return encode(m)
}

func jobsView(js []service.Job) ui.JobsView {
	var v ui.JobsView
	for _, j := range js {
		v.Rows = append(v.Rows, ui.JobRow{
			Kind:  j.Kind,
			State: j.State,
			When:  when(&j.CreatedAt),
			Error: j.Error,
		})
	}
	return v
}

func imageView(t service.Tile, i service.Image) ui.ImageView {
	return ui.ImageView{
		Ref:        t.ImageRef,
		Digest:     i.Digest,
		LastDigest: i.LastDigest,
		LastTag:    i.LastTag,
		Checked:    when(i.CheckedAt),
		LastError:  i.LastError,
	}
}

func runsView(base string, s service.TileStatus, rs []service.Run) ui.RunsView {
	v := ui.RunsView{Next: when(s.NextRun)}
	for _, r := range rs {
		row := ui.RunRow{
			ID:      r.ID,
			Status:  r.Status,
			Trigger: r.Trigger,
			Started: when(r.StartedAt),
			Live:    r.Status == "queued" || r.Status == "running",
		}
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
		v.Rows = append(v.Rows, ui.VolumeRow{
			ID:     x.ID,
			Name:   x.Name,
			State:  state,
			Drawer: env + "/-/volumes/" + x.ID + "?tab=backups",
		})
	}
	return v
}
