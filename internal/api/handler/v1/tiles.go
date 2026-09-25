package v1

import (
	"context"
	"encoding/json"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

// TilePatch is every field a caller may edit. A field left out stays as
// stored: only what is sent is applied (B1, B21, B22). Identity (id, stack,
// env, slug, kind) is not here; the name has its own route.
type TilePatch struct {
	GitURL                  *string  `json:"git_url,omitempty"`
	GitBranch               *string  `json:"git_branch,omitempty"`
	ImageRef                *string  `json:"image_ref,omitempty"`
	DockerfilePath          *string  `json:"dockerfile_path,omitempty"`
	BuildContext            *string  `json:"build_context,omitempty"`
	WatchPaths              *string  `json:"watch_paths,omitempty"`
	EnvJSON                 *string  `json:"env_json,omitempty"`
	BuildArgs               *string  `json:"build_args,omitempty"`
	Volumes                 *string  `json:"volumes,omitempty"`
	Command                 *string  `json:"command,omitempty"`
	ContainerPort           *int     `json:"container_port,omitempty"`
	PublishedPorts          *string  `json:"published_ports,omitempty"`
	EndpointProtocol        *string  `json:"endpoint_protocol,omitempty"`
	HealthPath              *string  `json:"health_path,omitempty"`
	HealthcheckCmd          *string  `json:"healthcheck_cmd,omitempty"`
	HealthcheckIntervalS    *int     `json:"healthcheck_interval_s,omitempty"`
	HealthcheckTimeoutS     *int     `json:"healthcheck_timeout_s,omitempty"`
	HealthcheckRetries      *int     `json:"healthcheck_retries,omitempty"`
	HealthcheckStartPeriodS *int     `json:"healthcheck_start_period_s,omitempty"`
	CPULimit                *float64 `json:"cpu_limit,omitempty"`
	MemLimitMB              *int     `json:"mem_limit_mb,omitempty"`
	User                    *string  `json:"user,omitempty"`
	ShmSizeMB               *int     `json:"shm_size_mb,omitempty"`
	Privileged              *bool    `json:"privileged,omitempty"`
	Devices                 *string  `json:"devices,omitempty"`
	RestartPolicy           *string  `json:"restart_policy,omitempty"`
	DependsOn               *string  `json:"depends_on,omitempty"`
	Files                   *string  `json:"files,omitempty"`
	SharedNet               *string  `json:"shared_net,omitempty"`
	Replicas                *int     `json:"replicas,omitempty"`
	UpdatePolicy            *string  `json:"update_policy,omitempty"`
	TagPolicy               *string  `json:"tag_policy,omitempty"`
	Schedule                *string  `json:"schedule,omitempty"`
	Trigger                 *string  `json:"trigger,omitempty"`
	TimeoutMinutes          *int     `json:"timeout_minutes,omitempty"`
}

// apply writes the sent fields onto t: the patch and the row share json
// names, so a round trip sets exactly the non-nil ones.
func (p TilePatch) apply(t *service.Tile) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, t)
}

type (
	TileIn struct {
		Name string `json:"name"`
		Kind string `json:"kind"` // service | image | cron | function
		TilePatch
	}
	ManagedIn struct {
		Name   string `json:"name"`
		Engine string `json:"engine"` // postgres | s3
		TilePatch
	}
	TileUpdated struct {
		Tile service.Tile `json:"tile"`
		Job  *service.Job `json:"job"` // the redeploy it queued, if running
	}
	LogOut struct {
		Log string `json:"log"`
	}
	RunStarted struct {
		Job *service.Job `json:"job"` // nil: refused, the last run is still going
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
	}
	RawCaddyIn struct {
		RawCaddy string `json:"raw_caddy"`
	}
	SliceIn struct {
		InstanceTileID string `json:"instance_tile_id"`
		Name           string `json:"name"`
		Public         bool   `json:"public"`
		OnRemove       string `json:"on_remove"`
	}
	ScopeIn struct {
		Scope string `json:"scope"`
	} // env | stack | org
)

func (in DomainIn) spec() service.DomainSpec {
	return service.DomainSpec{Host: in.Host, Path: in.Path, Port: in.Port, HTTPS: in.HTTPS, ForceHTTPS: in.ForceHTTPS,
		RedirectTo: in.RedirectTo, Extras: in.Extras}
}

func tileID(c echo.Context) string { return scope(c).Tile.ID }

// ---- tiles ----

func (h *H) Tiles() Endpoint {
	return Get(func(c echo.Context) ([]service.Tile, error) { return list(h.S.Tiles(rc(c), envID(c))) })
}

func (h *H) CreateTile() Endpoint {
	return JSON(201, func(c echo.Context, in TileIn) (service.Tile, error) {
		t := service.Tile{StackID: stackID(c), EnvironmentID: envID(c), Name: in.Name, Kind: in.Kind}
		if err := in.apply(&t); err != nil {
			return t, err
		}
		return h.S.CreateTile(rc(c), t)
	})
}

func (h *H) ManagedInstances() Endpoint {
	return Get(func(c echo.Context) ([]service.ManagedInstance, error) {
		return list(h.S.ManagedInstances(rc(c), envID(c)))
	})
}

func (h *H) CreateManagedTile() Endpoint {
	return JSON(201, func(c echo.Context, in ManagedIn) (service.Tile, error) {
		t := service.Tile{StackID: stackID(c), EnvironmentID: envID(c), Name: in.Name}
		if err := in.apply(&t); err != nil {
			return t, err
		}
		return h.S.CreateManagedTile(rc(c), t, in.Engine)
	})
}

func (h *H) GetTile() Endpoint {
	return Get(func(c echo.Context) (service.Tile, error) { return *scope(c).Tile, nil })
}

func (h *H) UpdateTile() Endpoint {
	return JSON(200, func(c echo.Context, in TilePatch) (TileUpdated, error) {
		t, j, err := h.S.UpdateTile(rc(c), tileID(c), in.apply)
		return TileUpdated{t, j}, err
	})
}

func (h *H) RenameTile() Endpoint {
	return JSON(200, func(c echo.Context, in NameIn) (service.Tile, error) {
		return h.S.RenameTile(rc(c), tileID(c), in.Name)
	})
}

func (h *H) DeleteTile() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return h.S.DeleteTile(rc(c), tileID(c)) })
}

// tileJob is the shape of every one-verb container op on a tile.
func tileJob(f func(ctx context.Context, id string) (service.Job, error)) Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return f(rc(c), tileID(c)) })
}

func (h *H) Deploy() Endpoint      { return tileJob(h.S.Deploy) }
func (h *H) RestartTile() Endpoint { return tileJob(h.S.RestartTile) }
func (h *H) StopTile() Endpoint    { return tileJob(h.S.StopTile) }
func (h *H) StartTile() Endpoint   { return tileJob(h.S.StartTile) }

func (h *H) CheckTileImages() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.S.CheckImages(rc(c), stackID(c), tileID(c))
	})
}

func (h *H) TileStatus() Endpoint {
	return Get(func(c echo.Context) (service.TileStatus, error) { return h.S.TileStatus(rc(c), tileID(c)) })
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
			s, err := h.S.RunLog(rc(c), tileID(c), run, tail)
			return LogOut{s}, err
		}
		s, err := h.S.Logs(rc(c), tileID(c), c.QueryParam("container"), tail)
		return LogOut{s}, err
	}).Q("container", "run", "tail")
}

// ---- runs (cron and function tiles) ----

func (h *H) RunTile() Endpoint {
	return JSON(202, func(c echo.Context, _ None) (RunStarted, error) {
		j, r, err := h.S.RunTile(rc(c), tileID(c))
		out := RunStarted{Run: r}
		if j.ID != "" {
			out.Job = &j
		}
		return out, err
	})
}

func (h *H) PauseTile() Endpoint {
	return JSON(200, func(c echo.Context, in PauseIn) (service.Tile, error) {
		return h.S.PauseTile(rc(c), tileID(c), in.Paused)
	})
}

// Runs is the tile's kept runs, newest first; ?limit= caps them.
func (h *H) Runs() Endpoint {
	return Get(func(c echo.Context) ([]service.Run, error) {
		limit := 20
		if err := echo.QueryParamsBinder(c).Int("limit", &limit).BindError(); err != nil {
			return nil, err
		}
		return list(h.S.Runs(rc(c), tileID(c), limit))
	}).Q("limit")
}

func (h *H) Run() Endpoint {
	return Get(func(c echo.Context) (service.Run, error) { return h.S.Run(rc(c), tileID(c), c.Param("run")) })
}

func (h *H) StopRun() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.S.StopRun(rc(c), tileID(c), c.Param("run")) })
}

// TileJobs is the tile's newest jobs; ?limit= caps them.
func (h *H) TileJobs() Endpoint {
	return Get(func(c echo.Context) ([]service.Job, error) {
		limit := 20
		if err := echo.QueryParamsBinder(c).Int("limit", &limit).BindError(); err != nil {
			return nil, err
		}
		return list(h.S.TileJobs(rc(c), []string{tileID(c)}, limit))
	}).Q("limit")
}

// ---- domains ----

func (h *H) Domains() Endpoint {
	return Get(func(c echo.Context) ([]service.Domain, error) { return list(h.S.Domains(rc(c), tileID(c))) })
}

func (h *H) AttachDomain() Endpoint {
	return JSON(201, func(c echo.Context, in DomainIn) (service.Domain, error) {
		return h.S.AttachDomain(rc(c), tileID(c), in.spec())
	})
}

func (h *H) UpdateDomain() Endpoint {
	return JSON(200, func(c echo.Context, in DomainIn) (service.Domain, error) {
		return h.S.UpdateDomain(rc(c), c.Param("domain"), in.spec())
	})
}

func (h *H) SetRawCaddy() Endpoint {
	return JSON(200, func(c echo.Context, in RawCaddyIn) (service.Domain, error) {
		return h.S.SetRawCaddy(rc(c), c.Param("domain"), in.RawCaddy)
	})
}

func (h *H) DetachDomain() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.S.DetachDomain(rc(c), c.Param("domain")) })
}

// ---- managed slices ----

func (h *H) Slices() Endpoint {
	return Get(func(c echo.Context) ([]service.Provision, error) { return list(h.S.Slices(rc(c), tileID(c))) })
}

func (h *H) AttachSlice() Endpoint {
	return Job(func(c echo.Context, in SliceIn) (service.Job, error) {
		return h.S.AttachSlice(rc(c), tileID(c), in.InstanceTileID, in.Name, in.Public, in.OnRemove)
	})
}

func (h *H) DetachSlice() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return h.S.DetachSlice(rc(c), c.Param("provision")) })
}

func (h *H) SetInstanceScope() Endpoint {
	return JSON(200, func(c echo.Context, in ScopeIn) (service.ManagedInstance, error) {
		return h.S.SetInstanceScope(rc(c), tileID(c), in.Scope)
	})
}
