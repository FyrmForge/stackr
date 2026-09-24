package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	ParamIn struct {
		Collection string `json:"collection"`
		Name       string `json:"name"`
		Kind       string `json:"kind"`  // param | secret
		Value      string `json:"value"` // a secret sent empty keeps its stored value (B35)
	}
	VolumeIn struct {
		Slug      string `json:"slug"`
		MaxSizeMB int    `json:"max_size_mb"`
	}
	ScheduleIn struct {
		Method   string  `json:"method"`
		DestID   *string `json:"dest_id"`
		Cron     string  `json:"cron"`
		Timezone string  `json:"timezone"`
		Keep     int     `json:"keep"`
		Mode     string  `json:"mode"`
	}
	BackupIn struct {
		DestID string `json:"dest_id"`
		Method string `json:"method"`
		Mode   string `json:"mode"`
	}
	RestoreIn struct {
		RunID          string `json:"run_id"`
		TargetVolumeID string `json:"target_volume_id"`
	}
	DestIn struct {
		Name      string `json:"name"`
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Bucket    string `json:"bucket"`
		AccessKey string `json:"access_key"` // "" keeps the stored one on update
		SecretKey string `json:"secret_key"` // "" keeps the stored one on update
		Shared    bool   `json:"shared"`
	}
)

func (in ScheduleIn) spec() service.ScheduleSpec {
	return service.ScheduleSpec{Method: in.Method, DestID: in.DestID, Cron: in.Cron, Timezone: in.Timezone, Keep: in.Keep, Mode: in.Mode}
}

func (in DestIn) spec() service.BackupDestSpec {
	return service.BackupDestSpec{Name: in.Name, Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket,
		AccessKey: in.AccessKey, SecretKey: in.SecretKey, Shared: in.Shared}
}

// At names the scope a params or volumes route works on.
type At func(c echo.Context) (kind, id string)

func AtOrg(c echo.Context) (string, string)   { return "org", orgID(c) }
func AtStack(c echo.Context) (string, string) { return "stack", stackID(c) }
func AtEnv(c echo.Context) (string, string)   { return "env", envID(c) }

func paramScope(c echo.Context, at At) service.ParamScope {
	k, id := at(c)
	return service.ParamScope{Kind: k, ID: id}
}

func volumeScope(c echo.Context, at At) service.VolumeScope {
	k, id := at(c)
	return service.VolumeScope{Kind: k, ID: id}
}

// ---- params ----

// Params lists params only; secrets have their own route and verb (B37).
func (h *H) Params(at At) Endpoint {
	return Get(func(c echo.Context) ([]service.Param, error) {
		return list(h.Orch.Params(rc(c), paramScope(c, at), false))
	})
}

func (h *H) Secrets(at At) Endpoint {
	return Get(func(c echo.Context) ([]service.Param, error) {
		return list(h.Orch.Params(rc(c), paramScope(c, at), true))
	})
}

// SetParams merges: entries not sent stay (B4).
func (h *H) SetParams(at At) Endpoint {
	return Done(func(c echo.Context, in []ParamIn) error {
		es := make([]service.ParamEntry, 0, len(in))
		for _, p := range in {
			es = append(es, service.ParamEntry(p))
		}
		return h.Orch.SetParams(rc(c), paramScope(c, at), es)
	})
}

func (h *H) DeleteParam(at At) Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeleteParam(rc(c), paramScope(c, at), c.Param("collection"), c.Param("name"))
	})
}

// ---- volumes and backups ----

func (h *H) Volumes(at At) Endpoint {
	return Get(func(c echo.Context) ([]service.Volume, error) { return list(h.Orch.Volumes(rc(c), volumeScope(c, at))) })
}

func (h *H) DeclareVolume(at At) Endpoint {
	return JSON(201, func(c echo.Context, in VolumeIn) (service.Volume, error) {
		return h.Orch.DeclareVolume(rc(c), volumeScope(c, at), in.Slug, in.MaxSizeMB)
	})
}

func (h *H) DeleteVolume() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteVolume(rc(c), c.Param("volume")) })
}

func (h *H) BackupMethods() Endpoint {
	return Get(func(c echo.Context) ([]string, error) { return list(h.Orch.BackupMethods(rc(c), c.Param("volume"))) })
}

func (h *H) BackupSchedules() Endpoint {
	return Get(func(c echo.Context) ([]service.BackupSchedule, error) {
		return list(h.Orch.BackupSchedules(rc(c), c.Param("volume")))
	})
}

func (h *H) AddBackupSchedule() Endpoint {
	return JSON(201, func(c echo.Context, in ScheduleIn) (service.BackupSchedule, error) {
		return h.Orch.AddBackupSchedule(rc(c), c.Param("volume"), in.spec())
	})
}

func (h *H) UpdateBackupSchedule() Endpoint {
	return JSON(200, func(c echo.Context, in ScheduleIn) (service.BackupSchedule, error) {
		return h.Orch.UpdateBackupSchedule(rc(c), c.Param("schedule"), in.spec())
	})
}

func (h *H) DeleteBackupSchedule() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteBackupSchedule(rc(c), c.Param("schedule")) })
}

func (h *H) BackupRuns() Endpoint {
	return Get(func(c echo.Context) ([]service.BackupRun, error) {
		return list(h.Orch.BackupRuns(rc(c), c.Param("volume")))
	})
}

func (h *H) BackupNow() Endpoint {
	return Job(func(c echo.Context, in BackupIn) (service.Job, error) {
		return h.Orch.BackupNow(rc(c), c.Param("volume"), in.DestID, in.Method, in.Mode)
	})
}

// RestoreBackup restores a run of the route's volume into the target, the
// same volume or another in the org.
func (h *H) RestoreBackup() Endpoint {
	return Job(func(c echo.Context, in RestoreIn) (service.Job, error) {
		return h.Orch.RestoreBackup(rc(c), in.RunID, c.Param("volume"), in.TargetVolumeID)
	})
}

// ---- backup destinations ----

func (h *H) BackupDests() Endpoint {
	return Get(func(c echo.Context) ([]service.BackupDest, error) { return list(h.Orch.BackupDests(rc(c), orgID(c))) })
}

func (h *H) CreateBackupDest() Endpoint {
	return JSON(201, func(c echo.Context, in DestIn) (service.BackupDest, error) {
		org := orgID(c)
		return h.Orch.CreateBackupDest(rc(c), &org, in.spec())
	})
}

func (h *H) UpdateBackupDest() Endpoint {
	return JSON(200, func(c echo.Context, in DestIn) (service.BackupDest, error) {
		return h.Orch.UpdateBackupDest(rc(c), orgID(c), c.Param("dest"), in.spec())
	})
}

func (h *H) DeleteBackupDest() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteBackupDest(rc(c), orgID(c), c.Param("dest")) })
}
