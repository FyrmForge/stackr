package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// The node verbs behind `stackr node`, `stackr cluster` and the env move and
// restore. Reads are open to any signed-in member (names, tags, states); the
// addresses and the writes are for server admins.

func isAdmin(c echo.Context) bool {
	p := middleware.Principal(c)
	return p != nil && authz.Can(p.Access, "node.manage", authz.Resource{}) == nil
}

// Nodes lists the nodes.
func (h *H) Nodes() Endpoint {
	return Get(func(c echo.Context) ([]service.NodeOut, error) {
		ns, err := h.Orch.ListNodes(rc(c))
		if !isAdmin(c) {
			for i := range ns {
				ns[i] = ns[i].Redacted()
			}
		}
		return list(ns, err)
	})
}

// GetNode is one node's system, certificate, envs and where it is an exit.
func (h *H) GetNode() Endpoint {
	return Get(func(c echo.Context) (service.NodeInfo, error) {
		d, err := h.Orch.GetNode(rc(c), c.Param("node"))
		if !isAdmin(c) {
			d = d.Redacted()
		}
		return d, err
	})
}

// TagNode (server admin) adds and removes capability tags.
func (h *H) TagNode() Endpoint {
	return JSON(200, func(c echo.Context, in service.TagSpec) (service.NodeOut, error) {
		return h.Orch.TagNode(rc(c), middleware.Principal(c), c.Param("node"), in)
	})
}

// NodeExitIn is the node's default exit ladder, node names top to bottom.
type NodeExitIn struct {
	Exits []string `json:"exits"`
}

// SetNodeExit (server admin) sets the default exit of a node without egress.
func (h *H) SetNodeExit() Endpoint {
	return JSON(200, func(c echo.Context, in NodeExitIn) (service.NodeOut, error) {
		return h.Orch.SetNodeExit(rc(c), middleware.Principal(c), c.Param("node"), in.Exits)
	})
}

// OrgIn names an org by slug.
type OrgIn struct {
	Org string `json:"org"`
}

// OpenNode (server admin) lets an org pin to the node.
func (h *H) OpenNode() Endpoint {
	return JSON(200, func(c echo.Context, in OrgIn) (service.NodeOut, error) {
		return h.Orch.OpenNode(rc(c), middleware.Principal(c), c.Param("node"), in.Org, true)
	})
}

// CloseNode (server admin) stops new pins from an org; its envs stay.
func (h *H) CloseNode() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (service.NodeOut, error) {
		return h.Orch.OpenNode(rc(c), middleware.Principal(c), c.Param("node"), c.Param("org"), false)
	})
}

// RejoinNode (server admin) answers a new join command for a broken worker.
func (h *H) RejoinNode() Endpoint {
	return JSON(201, func(c echo.Context, _ None) (service.JoinTokenOut, error) {
		return h.Orch.RejoinNode(rc(c), middleware.Principal(c), c.Param("node"))
	})
}

// RotateNodeKeys (server admin) starts a rotation of the node's mesh and
// exit keys; the outcome lands in the security log.
func (h *H) RotateNodeKeys() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RotateNodeKeys(rc(c), middleware.Principal(c), c.Param("node"))
	})
}

// RotateAllKeys (server admin) rotates every node's keys, one at a time.
func (h *H) RotateAllKeys() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RotateNodeKeys(rc(c), middleware.Principal(c), "")
	})
}

// KeyRotationIn is the rotation schedule in days, 0 = off.
type KeyRotationIn struct {
	Days int `json:"days"`
}

// KeyRotation is the rotation schedule.
func (h *H) KeyRotation() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (KeyRotationIn, error) {
		return KeyRotationIn{Days: h.Orch.RotateSchedule(rc(c))}, nil
	})
}

// SetKeyRotation (server admin) sets the rotation schedule.
func (h *H) SetKeyRotation() Endpoint {
	return JSON(200, func(c echo.Context, in KeyRotationIn) (KeyRotationIn, error) {
		return in, h.Orch.SetRotateSchedule(rc(c), middleware.Principal(c), in.Days)
	})
}

// RemoveNode (server admin) takes a worker with no envs out.
func (h *H) RemoveNode() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.RemoveNodeRef(rc(c), middleware.Principal(c), c.Param("node"))
	})
}

// ForgetNode (server admin) writes a dead worker off; its envs are lost.
func (h *H) ForgetNode() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (service.ForgetOut, error) {
		return h.Orch.ForgetNodeRef(rc(c), middleware.Principal(c), c.Param("node"))
	})
}

// LostEnvs lists the org's envs whose node was forgotten.
func (h *H) LostEnvs() Endpoint {
	return Get(func(c echo.Context) ([]service.NodeEnvOut, error) {
		return h.Orch.LostEnvsOf(rc(c), scope(c).Org.Slug)
	})
}

// ToIn names the target node.
type ToIn struct {
	To string `json:"to"`
}

// RestoreEnv re-homes a lost env onto a live node; the caller restores its
// volumes from the backups listed, then starts its tiles.
func (h *H) RestoreEnv() Endpoint {
	return JSON(200, func(c echo.Context, in ToIn) (service.RestoreOut, error) {
		return h.Orch.RestoreEnv(rc(c), middleware.Principal(c), envID(c), in.To)
	})
}

// MoveIn is a move: to a node, live unless cold.
type MoveIn struct {
	To   string `json:"to"`
	Cold bool   `json:"cold"`
}

// MoveEnv moves the env to another node as a job.
func (h *H) MoveEnv() Endpoint {
	return Job(func(c echo.Context, in MoveIn) (service.Job, error) {
		return h.Orch.MoveEnv(rc(c), middleware.Principal(c), envID(c), in.To, !in.Cold)
	})
}

// Cluster is the whole cluster grouped by network.
func (h *H) Cluster() Endpoint {
	return Get(func(c echo.Context) (service.ClusterOut, error) {
		out, err := h.Orch.ClusterStatus(rc(c))
		if !isAdmin(c) {
			out = out.Redacted()
		}
		return out, err
	})
}

// RegistryLogin mints the short-lived docker password for the stackr
// registry; it works nowhere else.
func (h *H) RegistryLogin() Endpoint {
	return JSON(201, func(c echo.Context, _ None) (service.RegistryLoginOut, error) {
		return h.Orch.RegistryLogin(rc(c), middleware.Principal(c))
	})
}

// NodeDefaults is the cluster-wide default ingress and exit.
func (h *H) NodeDefaults() Endpoint {
	return Get(func(c echo.Context) (service.DefaultsOut, error) {
		return h.Orch.Defaults(rc(c))
	})
}

// SetDefaultIngress (server admin) moves the default ingress.
func (h *H) SetDefaultIngress() Endpoint {
	return JSON(200, func(c echo.Context, in NodeIn) (service.DefaultsOut, error) {
		return h.Orch.SetDefaultIngress(rc(c), middleware.Principal(c), in.Node)
	})
}

// SetDefaultExit (server admin) moves or clears the default exit.
func (h *H) SetDefaultExit() Endpoint {
	return JSON(200, func(c echo.Context, in NodeIn) (service.DefaultsOut, error) {
		return h.Orch.SetDefaultExit(rc(c), middleware.Principal(c), in.Node)
	})
}
