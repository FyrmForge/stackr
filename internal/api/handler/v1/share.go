package v1

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	// ShareIn is a new network share of the org. User and PasswordRef are
	// ${{ org.params }} refs, never values.
	ShareIn struct {
		Slug        string `json:"slug"`
		Kind        string `json:"kind"` // nfs | smb
		Source      string `json:"source"`
		Options     string `json:"options"`
		User        string `json:"user"`
		PasswordRef string `json:"password_ref"`
	}
	// ShareOut is a share row.
	ShareOut struct {
		ID          string    `json:"id"`
		Slug        string    `json:"slug"`
		Kind        string    `json:"kind"`
		Source      string    `json:"source"`
		Options     string    `json:"options"`
		User        string    `json:"user"`
		PasswordRef string    `json:"password_ref"`
		CreatedAt   time.Time `json:"created_at"`
	}
)

func shareOut(s service.Share) ShareOut {
	return ShareOut{
		ID:          s.ID,
		Slug:        s.Slug,
		Kind:        s.Kind,
		Source:      s.Source,
		Options:     s.Options,
		User:        s.User,
		PasswordRef: s.PasswordRef,
		CreatedAt:   s.CreatedAt,
	}
}

func (h *H) Shares() Endpoint {
	return Get(func(c echo.Context) ([]ShareOut, error) {
		ss, err := h.Orch.Shares(rc(c), orgID(c))
		out := make([]ShareOut, 0, len(ss))
		for _, s := range ss {
			out = append(out, shareOut(s))
		}
		return list(out, err)
	})
}

func (h *H) CreateShare() Endpoint {
	return JSON(201, func(c echo.Context, in ShareIn) (ShareOut, error) {
		s, err := h.Orch.CreateShare(rc(c), orgID(c), service.ShareSpec{
			Slug:        in.Slug,
			Kind:        in.Kind,
			Source:      in.Source,
			Options:     in.Options,
			User:        in.User,
			PasswordRef: in.PasswordRef,
		})
		return shareOut(s), err
	})
}

// DeleteShare takes the share's slug or id; another org's is a 404.
func (h *H) DeleteShare() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeleteShare(rc(c), orgID(c), c.Param("share"))
	})
}
