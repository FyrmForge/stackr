package v1

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// The placement verbs. Members list the placements open to their org and
// never see a node name; everything under /admin/placements is for server
// admins, who also see the node a placement stands on.

// OfferOut is one row of the dev list: a placement open to the org.
type OfferOut struct {
	Name    string   `json:"name"`
	Tags    []string `json:"tags"`
	Public  bool     `json:"public"`
	Online  bool     `json:"online"`
	HasNode bool     `json:"has_node"`
	Usable  bool     `json:"usable"`
	Why     string   `json:"why,omitempty"` // "" when usable
}

// PlacementOut is a placement as an admin sees it.
type PlacementOut struct {
	Name          string         `json:"name"`
	Node          string         `json:"node"` // "" = no node yet
	Tags          []string       `json:"tags"`
	Public        bool           `json:"public"`
	Online        bool           `json:"online"`
	OpenToAll     bool           `json:"open_to_all"`
	OpenTo        []string       `json:"open_to"` // org slugs, when not open to all
	Envs          int            `json:"envs"`
	Default       bool           `json:"default"`
	NodeForgotten bool           `json:"node_forgotten"`
	Move          *MoveStatusOut `json:"move,omitempty"`
}

// CheckOut is one line of a move's checks.
type CheckOut struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MoveEnvOut is one env a move takes along.
type MoveEnvOut struct {
	Path     string `json:"path"`
	Volumes  int    `json:"volumes"`
	Bytes    int64  `json:"bytes"` // -1 = unknown
	Downtime string `json:"downtime"`
}

// MovePlanOut is the checks and the envs of a move before it runs.
type MovePlanOut struct {
	Placement string       `json:"placement"`
	From      string       `json:"from"`
	To        string       `json:"to"`
	Restore   bool         `json:"restore"` // the source was forgotten: restore from backups
	Envs      []MoveEnvOut `json:"envs"`
	Checks    []CheckOut   `json:"checks"`
	OK        bool         `json:"ok"`
}

// MoveStepOut is one line of an env's move progress.
type MoveStepOut struct {
	Label  string `json:"label"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// MoveEnvStatusOut is one env's move progress.
type MoveEnvStatusOut struct {
	From   string        `json:"from"`
	To     string        `json:"to"`
	Active bool          `json:"active"`
	Steps  []MoveStepOut `json:"steps"`
}

// MoveStatusOut is a running or finished repoint.
type MoveStatusOut struct {
	State string             `json:"state"` // running | failed | done
	From  string             `json:"from"`
	To    string             `json:"to"`
	Done  int                `json:"done"`
	All   int                `json:"all"`
	Error string             `json:"error,omitempty"`
	Envs  []MoveEnvStatusOut `json:"envs"`
}

func offerOut(o service.PlacementOffer) OfferOut {
	return OfferOut{Name: o.Name, Tags: nonNil(o.Tags), Public: o.Public, Online: o.Online, HasNode: o.HasNode, Usable: o.Usable, Why: o.Why}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func placementOut(p service.Placement) PlacementOut {
	return PlacementOut{
		Name: p.Name, Node: p.NodeName, Tags: nonNil(p.Tags), Public: p.Public, Online: p.Online,
		OpenToAll: p.OpenToAll, OpenTo: nonNil(p.Orgs), Envs: p.Envs, Default: p.Default,
		NodeForgotten: p.NodeForgotten, Move: moveStatusOut(p.Moving),
	}
}

func movePlanOut(p service.PlacementMovePlan) MovePlanOut {
	out := MovePlanOut{Placement: p.Placement, From: p.From, To: p.To, Restore: p.Restore, OK: p.OK, Envs: []MoveEnvOut{}, Checks: []CheckOut{}}
	for _, e := range p.Envs {
		out.Envs = append(out.Envs, MoveEnvOut{Path: e.Path, Volumes: e.Volumes, Bytes: e.Bytes, Downtime: e.Downtime})
	}
	for _, ck := range p.Checks {
		out.Checks = append(out.Checks, CheckOut{Name: ck.Name, OK: ck.OK, Detail: ck.Detail})
	}
	return out
}

func moveStatusOut(s *service.PlacementMoveStatus) *MoveStatusOut {
	if s == nil {
		return nil
	}
	out := &MoveStatusOut{State: s.State, From: s.From, To: s.To, Done: s.Done, All: s.All, Error: s.Error, Envs: []MoveEnvStatusOut{}}
	for _, e := range s.Envs {
		row := MoveEnvStatusOut{From: e.From, To: e.To, Active: e.Active, Steps: []MoveStepOut{}}
		for _, st := range e.Steps {
			row.Steps = append(row.Steps, MoveStepOut{Label: st.Label, State: st.State, Detail: st.Detail})
		}
		out.Envs = append(out.Envs, row)
	}
	return out
}

// OrgPlacements lists the placements open to the org: name, tags, public IP,
// online, usable. No node is named.
func (h *H) OrgPlacements() Endpoint {
	return Get(func(c echo.Context) ([]OfferOut, error) {
		os, err := h.Orch.OrgPlacements(rc(c), scope(c).Org.ID)
		out := make([]OfferOut, 0, len(os))
		for _, o := range os {
			out = append(out, offerOut(o))
		}
		return out, err
	})
}

// Placements (server admin) lists every placement with its node.
func (h *H) Placements() Endpoint {
	return Get(func(c echo.Context) ([]PlacementOut, error) {
		ps, err := h.Orch.Placements(rc(c))
		out := make([]PlacementOut, 0, len(ps))
		for _, p := range ps {
			out = append(out, placementOut(p))
		}
		return out, err
	})
}

// PlacementIn makes a placement. Node is a node's name, "" for none yet; no
// orgs means open to every org.
type PlacementIn struct {
	Name string   `json:"name"`
	Node string   `json:"node"`
	Orgs []string `json:"orgs"`
}

// CreatePlacement (server admin) makes a placement.
func (h *H) CreatePlacement() Endpoint {
	return JSON(http.StatusCreated, func(c echo.Context, in PlacementIn) (PlacementOut, error) {
		p, err := h.Orch.CreatePlacement(rc(c), middleware.Principal(c), service.PlacementSpec{
			Name: in.Name, Node: in.Node, OpenToAll: len(in.Orgs) == 0, Orgs: in.Orgs,
		})
		return placementOut(p), err
	})
}

// OpenIn is who may use a placement: every org, or the listed slugs.
type OpenIn struct {
	All  bool     `json:"all"`
	Orgs []string `json:"orgs"`
}

// SetPlacementOpen (server admin) opens a placement to all orgs or to a list;
// envs already there stay.
func (h *H) SetPlacementOpen() Endpoint {
	return Done(func(c echo.Context, in OpenIn) error {
		return h.Orch.SetPlacementOpen(rc(c), middleware.Principal(c), c.Param("name"), in.All, in.Orgs)
	})
}

// DeletePlacement (server admin) deletes a placement no pin or env uses.
func (h *H) DeletePlacement() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeletePlacement(rc(c), middleware.Principal(c), c.Param("name"))
	})
}

// PlacementMovePlan (server admin) is the checks and the envs of repointing
// the placement to ?to=<node>.
func (h *H) PlacementMovePlan() Endpoint {
	return Get(func(c echo.Context) (MovePlanOut, error) {
		p, err := h.Orch.PlanPlacementMove(rc(c), c.Param("name"), c.QueryParam("to"))
		return movePlanOut(p), err
	}).Q("to")
}

// PlacementMoveIn is a repoint: Confirm is the placement's name.
type PlacementMoveIn struct {
	To      string `json:"to"`
	Confirm string `json:"confirm"`
}

// MovePlacement (server admin) repoints the placement and starts the env
// moves; the job is not awaited.
func (h *H) MovePlacement() Endpoint {
	return Job(func(c echo.Context, in PlacementMoveIn) (service.Job, error) {
		return h.Orch.MovePlacement(rc(c), middleware.Principal(c), c.Param("name"), in.To, in.Confirm)
	})
}

// RetryPlacementMove (server admin) moves the envs a failed repoint left.
func (h *H) RetryPlacementMove() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.RetryPlacementMove(rc(c), middleware.Principal(c), c.Param("name"))
	})
}

// PlacementMove (server admin) is the placement's latest move; 404 = none.
func (h *H) PlacementMove() Endpoint {
	return Get(func(c echo.Context) (MoveStatusOut, error) {
		s, err := h.Orch.PlacementMove(rc(c), c.Param("name"))
		if err != nil {
			return MoveStatusOut{}, err
		}
		if s == nil {
			return MoveStatusOut{}, errs.ErrNotFound
		}
		return *moveStatusOut(s), nil
	})
}

// EnvPinIn pins an env to a placement ("" clears it). When that moves the
// env, Confirm must be the env's slug.
type EnvPinIn struct {
	Placement string `json:"placement"`
	Confirm   string `json:"confirm,omitempty"`
}

// EnvPinOut is the env's pin after the call; Job is set when the pin moved
// the env.
type EnvPinOut struct {
	Scope     string       `json:"scope"`
	Placement string       `json:"placement"`
	Job       *service.Job `json:"job,omitempty"`
}

// EnvPinPlan is the move that pinning the env to ?to=<placement> would cause:
// the checks and the env's line. No envs in it means the pin moves nothing.
func (h *H) EnvPinPlan() Endpoint {
	return Get(func(c echo.Context) (MovePlanOut, error) {
		p, err := h.Orch.PlanEnvPin(rc(c), envID(c), c.QueryParam("to"))
		return movePlanOut(p), err
	}).Q("to")
}

// SetEnvPin pins an env. A pin that moves the env answers 409 with the move
// plan, until the body's confirm is the env's slug; then the move starts.
func (h *H) SetEnvPin() Endpoint {
	e := JSON(http.StatusOK, func(echo.Context, EnvPinIn) (EnvPinOut, error) { return EnvPinOut{}, nil })
	e.Handle = func(c echo.Context) error {
		var in EnvPinIn
		if err := decode(c, &in); err != nil {
			return err
		}
		by, ctx, env := middleware.Principal(c), rc(c), scope(c).Env
		pin, err := h.Orch.PinNode(ctx, by, "env", env.ID, in.Placement)
		out := EnvPinOut{Scope: pin.Scope, Placement: in.Placement}
		var mn *service.MoveNeeded
		switch {
		case errors.As(err, &mn) && in.Confirm == env.Slug:
			j, err := h.Orch.MoveEnvToPlacement(ctx, by, env.ID, in.Placement, in.Confirm)
			if err != nil {
				return err
			}
			out.Job = &j
		case errors.As(err, &mn):
			return c.JSON(http.StatusConflict, movePlanOut(mn.Plan))
		case err != nil:
			return err
		}
		return c.JSON(http.StatusOK, out)
	}
	return e
}
