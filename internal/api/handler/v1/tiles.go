package v1

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// TilePatch is every field a caller may edit. A field left out stays as
// stored: only what is sent is applied (B1, B21, B22). Identity (id, stack,
// env, slug, kind) is not here; the name has its own route.
type TilePatch struct {
	GitURL                  *string          `json:"git_url,omitempty"`
	GitBranch               *string          `json:"git_branch,omitempty"`
	ImageRef                *string          `json:"image_ref,omitempty"`
	DockerfilePath          *string          `json:"dockerfile_path,omitempty"`
	BuildContext            *string          `json:"build_context,omitempty"`
	WatchPaths              *string          `json:"watch_paths,omitempty"`
	EnvJSON                 *json.RawMessage `json:"env_json,omitempty"`
	BuildArgs               *string          `json:"build_args,omitempty"`
	Volumes                 *json.RawMessage `json:"volumes,omitempty"`
	Command                 *string          `json:"command,omitempty"`
	ContainerPort           *int             `json:"container_port,omitempty"`
	PublishedPorts          *json.RawMessage `json:"published_ports,omitempty"`
	EndpointProtocol        *string          `json:"endpoint_protocol,omitempty"`
	HealthPath              *string          `json:"health_path,omitempty"`
	HealthcheckCmd          *string          `json:"healthcheck_cmd,omitempty"`
	HealthcheckIntervalS    *int             `json:"healthcheck_interval_s,omitempty"`
	HealthcheckTimeoutS     *int             `json:"healthcheck_timeout_s,omitempty"`
	HealthcheckRetries      *int             `json:"healthcheck_retries,omitempty"`
	HealthcheckStartPeriodS *int             `json:"healthcheck_start_period_s,omitempty"`
	CPULimit                *float64         `json:"cpu_limit,omitempty"`
	MemLimitMB              *int             `json:"mem_limit_mb,omitempty"`
	User                    *string          `json:"user,omitempty"`
	ShmSizeMB               *int             `json:"shm_size_mb,omitempty"`
	Privileged              *bool            `json:"privileged,omitempty"`
	Devices                 *string          `json:"devices,omitempty"`
	Lan                     *string          `json:"lan,omitempty"`
	HostNetwork             *bool            `json:"host_network,omitempty"`
	RestartPolicy           *string          `json:"restart_policy,omitempty"`
	DependsOn               *json.RawMessage `json:"depends_on,omitempty"`
	Files                   *json.RawMessage `json:"files,omitempty"`
	SharedNet               *string          `json:"shared_net,omitempty"`
	Replicas                *int             `json:"replicas,omitempty"`
	UpdatePolicy            *string          `json:"update_policy,omitempty"`
	TagPolicy               *string          `json:"tag_policy,omitempty"`
	Schedule                *string          `json:"schedule,omitempty"`
	Trigger                 *string          `json:"trigger,omitempty"`
	TimeoutMinutes          *int             `json:"timeout_minutes,omitempty"`
}

// apply writes the sent fields onto t: the patch and the row share json
// names, so a round trip sets exactly the non-nil ones. env_json, volumes,
// files, published_ports and depends_on take the structured form or the
// stored string.
func (p TilePatch) apply(t *service.Tile) error {
	cols := map[string]struct {
		in  **json.RawMessage
		out *string
	}{
		"env_json":        {&p.EnvJSON, &t.EnvJSON},
		"volumes":         {&p.Volumes, &t.Volumes},
		"files":           {&p.Files, &t.Files},
		"published_ports": {&p.PublishedPorts, &t.PublishedPorts},
		"depends_on":      {&p.DependsOn, &t.DependsOn},
	}
	raws := map[string]*json.RawMessage{}
	for k, c := range cols {
		raws[k], *c.in = *c.in, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, t); err != nil {
		return err
	}
	for k, raw := range raws {
		if raw == nil {
			continue
		}
		if *cols[k].out, err = service.TileColumn(k, *raw); err != nil {
			return err
		}
	}
	return nil
}

type (
	TileIn struct {
		Name string `json:"name"`
		Kind string `json:"kind"` // service | image | cron | function
		TilePatch
	}
	OperatedIn struct {
		Name   string `json:"name"`
		Engine string `json:"engine"` // postgres | s3
		Major  int    `json:"major"`  // postgres only: 14 to 18, 0 = the newest still supported
		TilePatch
	}
	TileUpdated struct {
		Tile service.TileView `json:"tile"`
		Job  *service.Job     `json:"job"` // the redeploy it queued, if running
	}
	LogOut struct {
		Log string `json:"log"`
	}
	RunStarted struct {
		Job *service.Job `json:"job"`
		Run service.Run  `json:"run"`
	}
	PauseIn struct {
		Paused bool `json:"paused"`
	}
	DomainIn struct {
		Host       string               `json:"host"`
		Path       string               `json:"path"`
		Port       int                  `json:"port"` // 0 = the tile's container port
		HTTPS      *bool                `json:"https"`
		ForceHTTPS *bool                `json:"force_https"`
		RedirectTo string               `json:"redirect_to"`
		Extras     service.DomainExtras `json:"proxy"`
		Auto       bool                 `json:"auto,omitempty"` // attach only: the generated name under the nearest domain resource; Host is ignored
		// Domain and Sub name the host in a managed domain (Sub "" = the
		// domain itself); Host is ignored when Domain is set.
		Domain string `json:"domain,omitempty"`
		Sub    string `json:"sub,omitempty"`
		// Replace takes over a DNS record stackr did not create on the
		// host's name (a 409 says when one is there).
		Replace bool `json:"replace,omitempty"`
	}
	RawCaddyIn struct {
		RawCaddy string `json:"raw_caddy"`
	}
	AllowIn struct {
		Allow []string `json:"allow"` // org:stack:env:tile patterns; the full list
	}
	PITRIn struct {
		Days int `json:"days"` // the recovery window in days; 0 = off
	}
	PGSettingsIn struct {
		Set   map[string]string `json:"set"`   // name to value; the rest stay
		Unset []string          `json:"unset"` // names whose override goes
	}
	PGSettingsOut struct {
		service.PGSettings
		Job *service.Job `json:"job,omitempty"` // the restart some changes need
	}
	EnvPairsIn struct {
		EnvPairs map[string]string `json:"env_pairs"` // consumer env -> this stack's env; the full map
	}
	SliceTileIn struct {
		Name          string `json:"name"`
		ProvisionFrom string `json:"provision_from"` // <stack>:<env>:<tile>
		DefaultAccess string `json:"default_access"` // read | write, "" = write
	}
	SliceAccessIn struct {
		Slice  string `json:"slice"`  // a slice tile's slug in the consumer's env
		Access string `json:"access"` // read | write | default (drops the entry: the slice's default applies)
	}
	OnRemoveIn struct {
		OnRemove string `json:"on_remove"` // keep | drop
	}
	SliceURLIn struct {
		URL string `json:"url"` // direct | pooled
	}
	BucketRestoreIn struct {
		At     time.Time `json:"at"`     // restore to how the objects were then (RFC3339)
		Prefix string    `json:"prefix"` // only keys under it; "" = the whole bucket
		Key    string    `json:"key"`    // only this one object; wins over prefix
	}
)

func (in DomainIn) spec() service.DomainSpec {
	if in.Domain != "" {
		in.Host = service.ManagedHost(in.Domain, in.Sub)
	}
	return service.DomainSpec{
		Host:       in.Host,
		Path:       in.Path,
		Port:       in.Port,
		HTTPS:      in.HTTPS,
		ForceHTTPS: in.ForceHTTPS,
		RedirectTo: in.RedirectTo,
		Extras:     in.Extras,
		Auto:       in.Auto,
	}
}

// tileView wraps a service answer: tileView(h.Orch.X(...)).
func tileView(t service.Tile, err error) (service.TileView, error) {
	return service.NewTileView(t), err
}

func tileID(c echo.Context) string { return scope(c).Tile.ID }

// ---- tiles ----

func (h *H) Tiles() Endpoint {
	return Get(func(c echo.Context) ([]service.TileView, error) {
		return service.TileViews(h.Orch.Tiles(rc(c), envID(c)))
	})
}

func (h *H) CreateTile() Endpoint {
	return JSON(201, func(c echo.Context, in TileIn) (service.TileView, error) {
		t := service.Tile{
			StackID:       stackID(c),
			EnvironmentID: envID(c),
			Name:          in.Name,
			Kind:          in.Kind,
		}
		if err := in.apply(&t); err != nil {
			return service.NewTileView(t), err
		}
		return tileView(h.Orch.CreateTile(rc(c), t))
	})
}

func (h *H) OperatedInstances() Endpoint {
	return Get(func(c echo.Context) ([]service.OperatedInstance, error) {
		return list(h.Orch.OperatedInstances(rc(c), envID(c)))
	})
}

func (h *H) CreateOperatedTile() Endpoint {
	return JSON(201, func(c echo.Context, in OperatedIn) (service.TileView, error) {
		t := service.Tile{StackID: stackID(c), EnvironmentID: envID(c), Name: in.Name}
		if err := in.apply(&t); err != nil {
			return service.NewTileView(t), err
		}
		return tileView(h.Orch.CreateOperatedTile(rc(c), t, in.Engine, in.Major))
	})
}

// OperatedSeeded is an operated tile made from a dump: the instance, the
// slice the dump loads into, and the job doing it.
type OperatedSeeded struct {
	Tile service.TileView `json:"tile"`
	Job  service.Job      `json:"job"`
}

// CreateOperatedFromDump makes a postgres operated tile from the request
// body, a pg_dump (custom or plain SQL), streamed to disk. ?slice= names
// the slice tile it loads into ("" = <name>-db). Past the 30s request
// timeout: the upload runs as long as the dump takes to arrive.
func (h *H) CreateOperatedFromDump() Endpoint {
	e := Endpoint{
		Status: http.StatusAccepted,
		Out:    reflect.TypeFor[OperatedSeeded](),
		InRaw:  "application/octet-stream",
		Handle: func(c echo.Context) error {
			t := service.Tile{StackID: stackID(c), EnvironmentID: envID(c), Name: c.QueryParam("name")}
			major, err := strconv.Atoi(cmp.Or(c.QueryParam("major"), "0"))
			if err != nil {
				return errs.Invalidf("major", "the major is a number such as 17")
			}
			out, j, err := h.Orch.CreateOperatedTileFromDump(
				context.WithoutCancel(rc(c)),
				t,
				c.QueryParam("engine"),
				c.QueryParam("slice"),
				major,
				middleware.Body(c),
			)
			if err != nil {
				return err
			}
			return c.JSON(http.StatusAccepted, OperatedSeeded{Tile: service.NewTileView(out), Job: j})
		},
	}
	return e.Q("name", "engine", "slice", "major")
}

func (h *H) GetTile() Endpoint {
	return Get(func(c echo.Context) (service.TileView, error) { return service.NewTileView(*scope(c).Tile), nil })
}

// TileUpdate is PATCH on a tile: the rename and every field of TilePatch;
// a field left out stays.
type TileUpdate struct {
	Name *string `json:"name,omitempty"`
	TilePatch
}

func (h *H) UpdateTile() Endpoint {
	return JSON(200, func(c echo.Context, in TileUpdate) (TileUpdated, error) {
		t := *scope(c).Tile
		if in.Name != nil {
			var err error
			if t, _, err = h.Orch.RenameTile(rc(c), tileID(c), *in.Name); err != nil { // the panel shows stack file warnings
				return TileUpdated{Tile: service.NewTileView(t)}, err
			}
		}
		if b, _ := json.Marshal(in.TilePatch); string(b) == "{}" {
			return TileUpdated{Tile: service.NewTileView(t)}, nil
		}
		t, j, err := h.Orch.UpdateTile(rc(c), tileID(c), in.apply)
		return TileUpdated{service.NewTileView(t), j}, err
	})
}

func (h *H) DeleteTile() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return h.Orch.DeleteTile(rc(c), tileID(c)) })
}

// tileJob is the shape of every one-verb container op on a tile.
func tileJob(f func(ctx context.Context, id string) (service.Job, error)) Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return f(rc(c), tileID(c)) })
}

func (h *H) Deploy() Endpoint {
	return tileJob(h.Orch.Deploy)
}

func (h *H) RebuildTile() Endpoint {
	return tileJob(h.Orch.RebuildTile)
}

func (h *H) RestartTile() Endpoint {
	return tileJob(h.Orch.RestartTile)
}

func (h *H) StopTile() Endpoint {
	return tileJob(h.Orch.StopTile)
}

func (h *H) StartTile() Endpoint {
	return tileJob(h.Orch.StartTile)
}

func (h *H) CheckTileImages() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.CheckImages(rc(c), stackID(c), tileID(c))
	})
}

func (h *H) TileStatus() Endpoint {
	return Get(func(c echo.Context) (service.TileStatus, error) { return h.Orch.TileStatus(rc(c), tileID(c)) })
}

// Logs is the tail of one replica; ?container= picks it ("" = the first),
// ?run= reads a cron or function run's log instead, ?tail= the line count.
func (h *H) Logs() Endpoint {
	return Get(func(c echo.Context) (LogOut, error) {
		tail := 200
		if err := echo.QueryParamsBinder(c).Int("tail", &tail).BindError(); err != nil {
			return LogOut{}, err
		}
		if run := c.QueryParam("run"); run != "" {
			s, err := h.Orch.RunLog(rc(c), tileID(c), run, tail)
			return LogOut{s}, err
		}
		s, err := h.Orch.Logs(rc(c), tileID(c), c.QueryParam("container"), tail)
		return LogOut{s}, err
	}).Q("container", "run", "tail")
}

// ---- runs (cron and function tiles) ----

func (h *H) RunTile() Endpoint {
	return JSON(202, func(c echo.Context, _ None) (RunStarted, error) {
		j, r, err := h.Orch.RunTile(rc(c), tileID(c))
		if err == nil && j.ID == "" { // the tile's last run is still going: nothing was queued
			return RunStarted{}, errs.Conflictf("the last run of this tile is still going")
		}
		return RunStarted{Run: r, Job: &j}, err
	}).Idempotent()
}

func (h *H) PauseTile() Endpoint {
	return JSON(200, func(c echo.Context, in PauseIn) (service.TileView, error) {
		return tileView(h.Orch.PauseTile(rc(c), tileID(c), in.Paused))
	})
}

// Runs is the tile's kept runs, newest first; ?limit= caps them.
func (h *H) Runs() Endpoint {
	return Get(func(c echo.Context) ([]service.Run, error) {
		limit := 20
		if err := echo.QueryParamsBinder(c).Int("limit", &limit).BindError(); err != nil {
			return nil, err
		}
		return list(h.Orch.Runs(rc(c), tileID(c), limit))
	}).Q("limit")
}

func (h *H) Run() Endpoint {
	return Get(func(c echo.Context) (service.Run, error) { return h.Orch.Run(rc(c), tileID(c), c.Param("run")) })
}

func (h *H) StopRun() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.StopRun(rc(c), tileID(c), c.Param("run")) })
}

// TileJobs is the tile's newest jobs; ?limit= caps them.
func (h *H) TileJobs() Endpoint {
	return Get(func(c echo.Context) ([]service.Job, error) {
		limit := 20
		if err := echo.QueryParamsBinder(c).Int("limit", &limit).BindError(); err != nil {
			return nil, err
		}
		return list(h.Orch.TileJobs(rc(c), []string{tileID(c)}, limit))
	}).Q("limit")
}

// ---- domains ----

func (h *H) Domains() Endpoint {
	return Get(func(c echo.Context) ([]service.Domain, error) { return list(h.Orch.Domains(rc(c), tileID(c))) })
}

// TileDNSDomains is the managed domains the tile's domains may sit in.
func (h *H) TileDNSDomains() Endpoint {
	return Get(func(c echo.Context) ([]string, error) { return list(h.Orch.TileDNSDomains(rc(c), tileID(c))) })
}

func (h *H) AttachDomain() Endpoint {
	return JSON(201, func(c echo.Context, in DomainIn) (service.Domain, error) {
		return h.Orch.AttachDomainReplace(rc(c), tileID(c), in.spec(), in.Replace)
	})
}

func (h *H) UpdateDomain() Endpoint {
	return JSON(200, func(c echo.Context, in DomainIn) (service.Domain, error) {
		return h.Orch.UpdateDomainReplace(rc(c), c.Param("domain"), in.spec(), in.Replace)
	})
}

func (h *H) SetRawCaddy() Endpoint {
	return JSON(200, func(c echo.Context, in RawCaddyIn) (service.Domain, error) {
		return h.Orch.SetRawCaddy(rc(c), c.Param("domain"), in.RawCaddy)
	})
}

func (h *H) DetachDomain() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DetachDomain(rc(c), c.Param("domain")) })
}

// DomainRecords is the custom DNS records declared on a tile domain's
// host, and where they are written.
func (h *H) DomainRecords() Endpoint {
	return Get(func(c echo.Context) (service.CustomRecords, error) {
		return h.Orch.TileDomainRecords(rc(c), tileID(c), c.Param("domain"))
	})
}

// AddDomainRecord declares a custom record on a tile domain's host; the
// answer's note says how it landed ("taken over", "added (1 other there
// stays)").
func (h *H) AddDomainRecord() Endpoint {
	return JSON(201, func(c echo.Context, in service.CustomRecordIn) (service.CustomRecord, error) {
		return h.Orch.AddTileDomainRecord(rc(c), tileID(c), c.Param("domain"), in)
	})
}

func (h *H) RemoveDomainRecord() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RemoveTileDomainRecord(rc(c), tileID(c), c.Param("domain"), c.Param("record"))
	})
}

// ---- operated tiles and slices ----

func (h *H) SetOperatedAllow() Endpoint {
	return JSON(200, func(c echo.Context, in AllowIn) (service.OperatedInstance, error) {
		return h.Orch.SetOperatedAllow(rc(c), tileID(c), in.Allow)
	})
}

// SetPITR sets the instance's recovery window and queues the redeploy that
// makes postgres archive (or stop).
func (h *H) SetPITR() Endpoint {
	return Job(func(c echo.Context, in PITRIn) (service.Job, error) {
		_, j, err := h.Orch.SetPITR(rc(c), tileID(c), in.Days)
		return j, err
	})
}

// InstanceRestoreIn is a point-in-time restore's time.
type InstanceRestoreIn struct {
	At string `json:"at"` // RFC3339 with an offset, or latest
}

// InstanceRestored is a restored candidate: its tile and the job that fills it.
type InstanceRestored struct {
	Tile service.TileView `json:"tile"`
	Job  service.Job      `json:"job"`
}

// RestoreInstance restores a postgres instance to a time into a new tile
// next to it; the job (202) fills and starts it. The source is untouched.
func (h *H) RestoreInstance() Endpoint {
	return JSON(http.StatusAccepted, func(c echo.Context, in InstanceRestoreIn) (InstanceRestored, error) {
		t, j, err := h.Orch.RestoreInstance(rc(c), middleware.Principal(c), tileID(c), in.At)
		return InstanceRestored{Tile: service.NewTileView(t), Job: j}, err
	})
}

// PromoteRestore swaps a restored copy's data under its source instance.
func (h *H) PromoteRestore() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.PromoteRestore(rc(c), middleware.Principal(c), tileID(c))
	})
}

// PGSettings lists a postgres instance's settings with where each value
// comes from.
func (h *H) PGSettings() Endpoint {
	return Get(func(c echo.Context) (service.PGSettings, error) { return h.Orch.PGSettings(rc(c), tileID(c)) })
}

// SetPGSettings sets and unsets the instance's own settings and applies them:
// a reload, or a restart (job) when a changed one needs it.
func (h *H) SetPGSettings() Endpoint {
	return JSON(200, func(c echo.Context, in PGSettingsIn) (PGSettingsOut, error) {
		v, j, err := h.Orch.SetPGSettings(rc(c), tileID(c), in.Set, in.Unset)
		return PGSettingsOut{PGSettings: v, Job: j}, err
	})
}

// PGVersion is the instance's postgres version, whether a newer minor of its
// major is out and whether its major is past end of life.
func (h *H) PGVersion() Endpoint {
	return Get(func(c echo.Context) (service.PGVersion, error) { return h.Orch.PGVersion(rc(c), tileID(c)) })
}

// UpdatePGVersion moves the instance to the current minor of its own major
// and restarts it there (job).
func (h *H) UpdatePGVersion() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (TileUpdated, error) {
		t, j, err := h.Orch.UpdatePGVersion(rc(c), tileID(c))
		return TileUpdated{service.NewTileView(t), j}, err
	})
}

func (h *H) PITRStatus() Endpoint {
	return Get(func(c echo.Context) (service.PITRHealth, error) { return h.Orch.PITRStatus(rc(c), tileID(c)) })
}

func (h *H) SetOperatedEnvPairs() Endpoint {
	return JSON(200, func(c echo.Context, in EnvPairsIn) (service.OperatedInstance, error) {
		return h.Orch.SetOperatedEnvPairs(rc(c), tileID(c), in.EnvPairs)
	})
}

func (h *H) CreateSliceTile() Endpoint {
	return JSON(201, func(c echo.Context, in SliceTileIn) (service.TileView, error) {
		return tileView(h.Orch.CreateSliceTile(rc(c), envID(c), in.Name, in.ProvisionFrom, in.DefaultAccess))
	})
}

// SliceBranched is a branch: its slice tile and the job that loads it.
type SliceBranched struct {
	Tile service.TileView `json:"tile"`
	Job  service.Job      `json:"job"`
}

// BranchSlice copies a postgres slice as of now into a new slice tile;
// the job (202) provisions it and loads the data.
func (h *H) BranchSlice() Endpoint {
	return JSON(http.StatusAccepted, func(c echo.Context, in service.BranchIn) (SliceBranched, error) {
		t, j, err := h.Orch.BranchSlice(rc(c), middleware.Principal(c), tileID(c), in)
		return SliceBranched{Tile: service.NewTileView(t), Job: j}, err
	})
}

// SliceCreds reveals the slice's own owner credentials (logged).
func (h *H) SliceCreds() Endpoint {
	return Get(func(c echo.Context) (service.SliceCreds, error) {
		return h.Orch.SliceCreds(rc(c), middleware.Principal(c), tileID(c))
	})
}

func (h *H) SetSliceAccess() Endpoint {
	return JSON(200, func(c echo.Context, in SliceAccessIn) (service.TileView, error) {
		return tileView(h.Orch.SetSliceAccess(rc(c), tileID(c), in.Slice, in.Access))
	})
}

func (h *H) SetSliceOnRemove() Endpoint {
	return JSON(200, func(c echo.Context, in OnRemoveIn) (service.TileView, error) {
		return tileView(h.Orch.SetSliceOnRemove(rc(c), tileID(c), in.OnRemove))
	})
}

func (h *H) SetSliceURL() Endpoint {
	return JSON(200, func(c echo.Context, in SliceURLIn) (service.TileView, error) {
		return tileView(h.Orch.SetSliceURL(rc(c), tileID(c), in.URL))
	})
}

// SetSliceBucket replaces a bucket slice's versioning, quota and CORS.
func (h *H) SetSliceBucket() Endpoint {
	return JSON(200, func(c echo.Context, in service.BucketSettings) (service.TileView, error) {
		return tileView(h.Orch.SetSliceBucket(rc(c), tileID(c), in))
	})
}

// SliceUsage is a bucket's current bytes and old-version bytes.
func (h *H) SliceUsage() Endpoint {
	return Get(func(c echo.Context) (service.BucketUsage, error) { return h.Orch.SliceUsage(rc(c), tileID(c)) })
}

// RestoreSlice queues a restore of a bucket to a past time: it copies the
// version current then back on top and never deletes.
func (h *H) RestoreSlice() Endpoint {
	return Job(func(c echo.Context, in BucketRestoreIn) (service.Job, error) {
		if in.Key != "" {
			return h.Orch.RestoreSlice(rc(c), tileID(c), in.Key, true, in.At)
		}
		return h.Orch.RestoreSlice(rc(c), tileID(c), in.Prefix, false, in.At)
	})
}

func (h *H) Bindings() Endpoint {
	return Get(func(c echo.Context) ([]service.SliceBinding, error) {
		return list(h.Orch.Bindings(rc(c), tileID(c)))
	})
}

func (h *H) SliceOf() Endpoint {
	return Get(func(c echo.Context) (service.SliceView, error) { return h.Orch.SliceOf(rc(c), tileID(c)) })
}

type (
	StandbyIn struct {
		Node string `json:"node"` // a node's name, slug or id
	}
)

type StandbySyncIn struct {
	Mode string `json:"mode"` // auto, always or never
}

type (
	SwitchoverIn struct {
		To string `json:"to"` // the standby's node; empty = the one that replayed the most
	}
	PromoteStandbyIn struct {
		To       string `json:"to"`        // the standby's node
		NodeGone bool   `json:"node_gone"` // the old primary's node is confirmed gone
		// PoweredOff is service.PoweredOffPhrase: promote without a lease proof.
		PoweredOff string `json:"powered_off"`
		// AllowAsyncTarget promotes a standby the primary did not wait for
		// although one it did wait for is usable.
		AllowAsyncTarget bool `json:"allow_async_target"`
	}
)

// Switchover moves the primary to a standby while both run: the archive is
// drained, the primary shut down cleanly, the standby confirmed to have
// replayed the shutdown checkpoint, then promoted. It answers when done.
func (h *H) Switchover() Endpoint {
	return JSON(http.StatusOK, func(c echo.Context, in SwitchoverIn) (service.StandbyView, error) {
		return h.Orch.Switchover(rc(c), tileID(c), in.To)
	})
}

// PromoteStandby makes a standby the primary when the primary is down. The
// old primary must be known stopped; for a node that does not answer, the
// caller confirms it is really gone (node_gone), the panel needs its record
// that the primary was under its worker's lease, or the caller types the
// powered_off phrase.
func (h *H) PromoteStandby() Endpoint {
	return JSON(http.StatusOK, func(c echo.Context, in PromoteStandbyIn) (service.StandbyView, error) {
		return h.Orch.PromoteStandby(rc(c), tileID(c), in.To, in.NodeGone, in.PoweredOff, in.AllowAsyncTarget)
	})
}

// SyncOf is the instance's synchronous replication and whether its writes
// are protected now.
func (h *H) SyncOf() Endpoint {
	return Get(func(c echo.Context) (service.SyncView, error) { return h.Orch.SyncOf(rc(c), tileID(c)) })
}

// SetSync changes whether the instance prefers synchronous replication, the
// round trip limit and whether it is required.
func (h *H) SetSync() Endpoint {
	return JSON(http.StatusOK, func(c echo.Context, in service.SetSyncIn) (service.SyncView, error) {
		return h.Orch.SetSync(rc(c), tileID(c), in)
	})
}

// SetStandbySync sets how the primary treats one standby: auto, always or never.
func (h *H) SetStandbySync() Endpoint {
	return JSON(http.StatusOK, func(c echo.Context, in StandbySyncIn) (service.SyncView, error) {
		return h.Orch.SetStandbySync(rc(c), tileID(c), c.Param("standby"), in.Mode)
	})
}

// Standbys lists an instance's members: the primary, then its standbys.
func (h *H) Standbys() Endpoint {
	return Get(func(c echo.Context) ([]service.StandbyView, error) { return h.Orch.Standbys(rc(c), tileID(c)) })
}

// AddStandby makes the instance a standby on a node; it is streaming once
// the copy of the primary is done.
func (h *H) AddStandby() Endpoint {
	return JSON(http.StatusAccepted, func(c echo.Context, in StandbyIn) (service.StandbyView, error) {
		return h.Orch.AddStandbyView(rc(c), tileID(c), in.Node)
	})
}

// RemoveStandby takes a standby away: its container, volume, slot and network.
func (h *H) RemoveStandby() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RemoveStandbyOf(rc(c), tileID(c), c.Param("standby"))
	})
}

// RebuildStandby makes a standby again on its node (one whose slot was lost).
func (h *H) RebuildStandby() Endpoint {
	return JSON(http.StatusAccepted, func(c echo.Context, _ None) (service.StandbyView, error) {
		return h.Orch.RebuildStandbyOf(rc(c), tileID(c), c.Param("standby"))
	})
}
