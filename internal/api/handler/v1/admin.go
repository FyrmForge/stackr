package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	AdminIn struct {
		Admin bool `json:"admin"`
	}
	SetPassIn struct {
		Password string `json:"password"`
	}
	ValueIn struct {
		Value string `json:"value"`
	}
	TagIn struct {
		Tag string `json:"tag"`
	}
	VersionOut struct {
		Version string `json:"version"`
	}
	UpgradeOut struct {
		Latest string `json:"latest"`
		Newer  bool   `json:"newer"`
	}
)

func (h *H) AllOrgs() Endpoint {
	return Get(func(c echo.Context) ([]service.Org, error) { return list(h.S.AllOrgs(rc(c))) })
}

func (h *H) Users() Endpoint {
	return Get(func(c echo.Context) ([]service.User, error) { return list(h.S.Users(rc(c))) })
}

func (h *H) SetAdmin() Endpoint {
	return Done(func(c echo.Context, in AdminIn) error { return h.S.SetAdmin(rc(c), c.Param("user"), in.Admin) })
}

func (h *H) DisableUser() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.S.DisableUser(rc(c), c.Param("user")) })
}

func (h *H) SetPassword() Endpoint {
	return Done(func(c echo.Context, in SetPassIn) error { return h.S.SetPassword(rc(c), c.Param("user"), in.Password) })
}

func (h *H) Images() Endpoint {
	return Get(func(c echo.Context) ([]service.Image, error) { return list(h.S.Images(rc(c))) })
}

func (h *H) CheckAllImages() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return h.S.CheckImages(rc(c), "", "") })
}

func (h *H) Version() Endpoint {
	return Get(func(c echo.Context) (VersionOut, error) { return VersionOut{h.S.Version()}, nil })
}

func (h *H) CheckUpgrade() Endpoint {
	return Get(func(c echo.Context) (UpgradeOut, error) {
		latest, newer, err := h.S.CheckUpgrade(rc(c))
		return UpgradeOut{latest, newer}, err
	})
}

func (h *H) Upgrade() Endpoint {
	return Job(func(c echo.Context, in TagIn) (service.Job, error) { return h.S.Upgrade(rc(c), in.Tag) })
}

func (h *H) PanelBackups() Endpoint {
	return Get(func(c echo.Context) ([]service.BackupRun, error) { return list(h.S.PanelBackups(rc(c))) })
}

func (h *H) PanelBackupNow() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return h.S.PanelBackupNow(rc(c)) })
}

func (h *H) SyncProxy() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.S.SyncProxy(rc(c)) })
}

func (h *H) Setting() Endpoint {
	return Get(func(c echo.Context) (ValueIn, error) {
		v, err := h.S.Setting(rc(c), c.Param("setting"))
		return ValueIn{v}, err
	})
}

func (h *H) SetSetting() Endpoint {
	return Done(func(c echo.Context, in ValueIn) error { return h.S.SetSetting(rc(c), c.Param("setting"), in.Value) })
}

func (h *H) SettingDefaults() Endpoint {
	return Get(func(c echo.Context) (service.SettingsBlob, error) { return h.S.SettingDefaults(rc(c)) })
}

// SetSettingDefaults merges the sent knobs into the server rung.
func (h *H) SetSettingDefaults() Endpoint {
	return Done(func(c echo.Context, in map[string]string) error { return h.S.SetSettingDefaults(rc(c), in) })
}

func (h *H) GlobalBackupDests() Endpoint {
	return Get(func(c echo.Context) ([]service.BackupDest, error) { return list(h.S.GlobalBackupDests(rc(c))) })
}

func (h *H) CreateGlobalBackupDest() Endpoint {
	return JSON(201, func(c echo.Context, in DestIn) (service.BackupDest, error) {
		return h.S.CreateBackupDest(rc(c), nil, in.spec())
	})
}

func (h *H) UpdateGlobalBackupDest() Endpoint {
	return JSON(200, func(c echo.Context, in DestIn) (service.BackupDest, error) {
		return h.S.UpdateBackupDest(rc(c), "", c.Param("dest"), in.spec())
	})
}

func (h *H) DeleteGlobalBackupDest() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.S.DeleteBackupDest(rc(c), "", c.Param("dest")) })
}
