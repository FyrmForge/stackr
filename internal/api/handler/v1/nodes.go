package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// The node verbs behind `stackr node` and `stackr cluster`. Nodes are
// admin-only hardware: every route here is for server admins, and members see
// placements instead (placements.go).

// Nodes lists the nodes.
func (h *H) Nodes() Endpoint {
	return Get(func(c echo.Context) ([]service.NodeOut, error) {
		return list(h.Orch.ListNodes(rc(c)))
	})
}

// GetNode is one node's system, certificate, envs and where it is an exit.
func (h *H) GetNode() Endpoint {
	return Get(func(c echo.Context) (service.NodeInfo, error) {
		return h.Orch.GetNode(rc(c), c.Param("node"))
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

// NodePublicIPIn is the address a node's DNS records point at, "" = the
// detected one.
type NodePublicIPIn struct {
	PublicIP string `json:"public_ip"`
}

// SetNodePublicIP (server admin) sets a node's public IP override.
func (h *H) SetNodePublicIP() Endpoint {
	return JSON(200, func(c echo.Context, in NodePublicIPIn) (service.NodeOut, error) {
		return h.Orch.SetNodePublicIP(rc(c), middleware.Principal(c), c.Param("node"), in.PublicIP)
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

// RemoveNode (server admin) queues the removal of a worker with no envs: stop,
// back up, check, clean the box, forget the node. Follow the job.
func (h *H) RemoveNode() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.RemoveNodeRef(rc(c), middleware.Principal(c), c.Param("node"))
	})
}

// BackupNode (server admin) queues a backup of every volume on the node.
func (h *H) BackupNode() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.BackupNodeRef(rc(c), middleware.Principal(c), c.Param("node"))
	})
}

// NodeBackups (server admin) lists the node's backup groups, newest first.
func (h *H) NodeBackups() Endpoint {
	return Get(func(c echo.Context) ([]service.BackupGroupView, error) {
		gs, err := h.Orch.NodeBackupsRef(rc(c), c.Param("node"))
		return list(gs, err)
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

// Cluster is the whole cluster grouped by network.
func (h *H) Cluster() Endpoint {
	return Get(func(c echo.Context) (service.ClusterOut, error) {
		return h.Orch.ClusterStatus(rc(c))
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

// DefaultIngressIn is the new default ingress; Confirm is the node's name.
type DefaultIngressIn struct {
	Node    string `json:"node"`
	Confirm string `json:"confirm"`
}

// DefaultIngressPlan (server admin) lists the hosts that would enter by ?node=
// and where each points now.
func (h *H) DefaultIngressPlan() Endpoint {
	return Get(func(c echo.Context) (service.IngressPlan, error) {
		return h.Orch.IngressSwitchPlan(rc(c), middleware.Principal(c), c.QueryParam("node"))
	})
}

// SetDefaultIngress (server admin) moves the default ingress.
func (h *H) SetDefaultIngress() Endpoint {
	return JSON(200, func(c echo.Context, in DefaultIngressIn) (service.DefaultsOut, error) {
		return h.Orch.SetDefaultIngress(rc(c), middleware.Principal(c), in.Node, in.Confirm)
	})
}

// NodeIn names a node; "" clears the pick.
type NodeIn struct {
	Node string `json:"node"`
}

// SetDefaultExit (server admin) moves or clears the default exit.
func (h *H) SetDefaultExit() Endpoint {
	return JSON(200, func(c echo.Context, in NodeIn) (service.DefaultsOut, error) {
		return h.Orch.SetDefaultExit(rc(c), middleware.Principal(c), in.Node)
	})
}
