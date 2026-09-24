package v1

import (
	"encoding/json"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	StackIn struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	ConfigRepoIn struct {
		ConnectorID string `json:"connector_id"`
		Repo        string `json:"repo"`
		Branch      string `json:"branch"`
		Path        string `json:"path"`
	}
	ReservationIn struct {
		Host                string `json:"host"`
		ACMEEmail           string `json:"acme_email"`
		IncludeEnvOnDefault bool   `json:"include_env_on_default"`
	}
	// Blob is a settings rung, a JSON object the service checks.
	Blob       = json.RawMessage
	ReleaseOut struct {
		Release service.Release        `json:"release"`
		Pins    map[string]service.Pin `json:"pins"`
	}
	EnvIn struct {
		Name       string  `json:"name"`
		Type       string  `json:"type"` // static | ephemeral
		Base       *string `json:"base_env_id"`
		Color      string  `json:"color"`
		FromKind   string  `json:"from_kind"` // branch | promote
		FromBranch string  `json:"from_branch"`
		Auto       bool    `json:"auto"`
	}
	ColorIn struct {
		Color string `json:"color"`
	}
	FromIn struct {
		Kind   string `json:"kind"`
		Branch string `json:"branch"`
		Auto   bool   `json:"auto"`
	}
	OrderIn struct {
		IDs []string `json:"ids"`
	} // bottom rung first
)

func stackID(c echo.Context) string { return scope(c).Stack.ID }
func envID(c echo.Context) string   { return scope(c).Env.ID }

// ---- stacks ----

func (h *H) Stacks() Endpoint {
	return Get(func(c echo.Context) ([]service.Stack, error) { return list(h.Orch.Stacks(rc(c), orgID(c))) })
}

func (h *H) CreateStack() Endpoint {
	return JSON(201, func(c echo.Context, in StackIn) (service.Stack, error) {
		return h.Orch.CreateStack(rc(c), orgID(c), in.Name, in.Description)
	})
}

func (h *H) GetStack() Endpoint {
	return Get(func(c echo.Context) (service.Stack, error) { return *scope(c).Stack, nil })
}

func (h *H) RenameStack() Endpoint {
	return JSON(200, func(c echo.Context, in NameIn) (service.Stack, error) {
		return h.Orch.RenameStack(rc(c), stackID(c), in.Name)
	})
}

func (h *H) SetConfigRepo() Endpoint {
	return JSON(200, func(c echo.Context, in ConfigRepoIn) (service.Stack, error) {
		return h.Orch.SetConfigRepo(rc(c), stackID(c), in.ConnectorID, in.Repo, in.Branch, in.Path)
	})
}

func (h *H) SetReservations() Endpoint {
	return JSON(200, func(c echo.Context, in []ReservationIn) (service.Stack, error) {
		rs := make([]service.Reservation, 0, len(in))
		for _, r := range in {
			rs = append(rs, service.Reservation(r))
		}
		return h.Orch.SetReservations(rc(c), stackID(c), rs)
	})
}

func (h *H) SetStackSettings() Endpoint {
	return JSON(200, func(c echo.Context, in Blob) (service.Stack, error) {
		return h.Orch.SetStackSettings(rc(c), stackID(c), string(in))
	})
}

func (h *H) DeleteStack() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteStack(rc(c), stackID(c)) })
}

func (h *H) CheckStackImages() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return h.Orch.CheckImages(rc(c), stackID(c), "") })
}

// ---- releases and promote ----

func (h *H) Releases() Endpoint {
	return Get(func(c echo.Context) ([]service.Release, error) { return list(h.Orch.Releases(rc(c), stackID(c))) })
}

func (h *H) Release() Endpoint {
	return Get(func(c echo.Context) (ReleaseOut, error) {
		r, pins, err := h.Orch.Release(rc(c), c.Param("release"))
		return ReleaseOut{r, pins}, err
	})
}

// PlanPromote is the dry run; its blockers are the ones Promote and
// Rollback refuse with (B2, B20).
func (h *H) PlanPromote() Endpoint {
	return Get(func(c echo.Context) (service.PromotePlan, error) {
		return h.Orch.PlanPromote(rc(c), envID(c), c.Param("release"))
	})
}

func (h *H) Promote() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.Promote(rc(c), envID(c), c.Param("release"))
	})
}

func (h *H) Rollback() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.Rollback(rc(c), envID(c), c.Param("release"))
	})
}

// ---- envs ----

func (h *H) Envs() Endpoint {
	return Get(func(c echo.Context) ([]service.Environment, error) { return list(h.Orch.Envs(rc(c), stackID(c))) })
}

func (h *H) Ladder() Endpoint {
	return Get(func(c echo.Context) ([]service.Environment, error) { return list(h.Orch.Ladder(rc(c), stackID(c))) })
}

func (h *H) CreateEnv() Endpoint {
	return JSON(201, func(c echo.Context, in EnvIn) (service.Environment, error) {
		return h.Orch.CreateEnv(rc(c), stackID(c), in.Name, service.EnvSpec{Type: in.Type, Base: in.Base, Color: in.Color,
			FromKind: in.FromKind, FromBranch: in.FromBranch, Auto: in.Auto})
	})
}

func (h *H) ReorderEnvs() Endpoint {
	return Done(func(c echo.Context, in OrderIn) error { return h.Orch.ReorderEnvs(rc(c), stackID(c), in.IDs) })
}

func (h *H) GetEnv() Endpoint {
	return Get(func(c echo.Context) (service.Environment, error) { return *scope(c).Env, nil })
}

// Traffic is the env's tile-to-tile lanes at the last sample.
func (h *H) Traffic() Endpoint {
	return Get(func(c echo.Context) ([]service.Edge, error) { return list(h.Orch.Traffic(rc(c), envID(c))) })
}

func (h *H) RenameEnv() Endpoint {
	return JSON(200, func(c echo.Context, in NameIn) (service.Environment, error) {
		return h.Orch.RenameEnv(rc(c), envID(c), in.Name)
	})
}

func (h *H) SetEnvColor() Endpoint {
	return JSON(200, func(c echo.Context, in ColorIn) (service.Environment, error) {
		return h.Orch.SetEnvColor(rc(c), envID(c), in.Color)
	})
}

func (h *H) SetEnvFrom() Endpoint {
	return JSON(200, func(c echo.Context, in FromIn) (service.Environment, error) {
		return h.Orch.SetEnvFrom(rc(c), envID(c), in.Kind, in.Branch, in.Auto)
	})
}

func (h *H) SetEnvSettings() Endpoint {
	return JSON(200, func(c echo.Context, in Blob) (service.Environment, error) {
		return h.Orch.SetEnvSettings(rc(c), envID(c), string(in))
	})
}

func (h *H) DeleteEnv() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteEnv(rc(c), envID(c)) })
}
