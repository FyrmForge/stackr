package tile

import (
	"context"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/tile"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// LiveEvery is how often an open drawer's stream reads the tile again (the
// canvas stream's pace: a read asks Docker).
var LiveEvery = 3 * time.Second

// GET …/-/tiles/:tile/live?part=head|status: the open drawer's stream, the
// instance drawer's polling stream. One event per region: every region once
// at connect, then only when its markup changes. part=head is the header's
// (the buttons, and "word" when the status word or pause changes, which
// draws the drawer again for its badge); part=status is the Status tab's
// (status, lastjob, urls, access).
func (h *handler) Live(c echo.Context) error {
	t, env, b := *tileOf(c), render.EnvURL(c), base(c)
	head := c.QueryParam("part") == "head"
	v := ui.View{Kind: t.Kind, Name: t.Name, Base: b, ReadOnly: !can(c, "tile.write"), Rebuild: service.Rebuildable(t)}
	sent := map[string]stream.HTML{}
	var last time.Time
	var word string
	return stream.Watch(c, func(ctx context.Context) ([]stream.Msg, error) {
		if time.Since(last) < LiveEvery {
			return nil, nil
		}
		last = time.Now()
		st, err := h.orch.TileStatus(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		var out []stream.Msg
		var regions map[string]templ.Component
		var order []string
		if head {
			v.Status, v.Stopped, v.Paused = st.Word, st.Word == "stopped", st.Paused
			regions, order = map[string]templ.Component{"actions": ui.LiveActions(v)}, []string{"actions"}
			w := st.Word
			if st.Paused {
				w += "/paused"
			}
			if word != "" && w != word {
				out = append(out, stream.Msg{Name: "word", Body: stream.HTML(w)})
			}
			word = w
		} else {
			sv, err := h.statusOf(ctx, env, b, t, st)
			if err != nil {
				return nil, err
			}
			regions = map[string]templ.Component{
				"status": ui.LiveStatus(v, sv), "lastjob": ui.LiveJob(v, sv),
				"urls": ui.LiveURLs(v, sv), "access": ui.LiveAccess(sv),
			}
			order = []string{"status", "lastjob", "urls", "access"}
		}
		for _, name := range order {
			body, err := render.Event(ctx, regions[name])
			if err != nil {
				return nil, err
			}
			if was, ok := sent[name]; !ok || was != body {
				sent[name] = body
				out = append(out, stream.Msg{Name: name, Body: body})
			}
		}
		return out, nil
	})
}
