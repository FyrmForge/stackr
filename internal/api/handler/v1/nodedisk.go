package v1

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// A node's disk: the verbs behind `stkr node disk` and `stkr volume
// move-to-disk`. The node verbs are server-admin; the volume verbs are
// tile.write in the volume's org. The service holds every rule and refusal.

// nodeID resolves the :node param (name, id or id prefix) to the node's id.
func (h *H) nodeID(c echo.Context) (string, error) {
	n, err := h.Orch.GetNode(rc(c), c.Param("node"))
	return n.ID, err
}

// NodeDiskOut is the node's disk; Has false means it has none.
type NodeDiskOut struct {
	Has     bool      `json:"has"`
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Path    string    `json:"path"`
	State   string    `json:"state"`
	Free    uint64    `json:"free"`
	Total   uint64    `json:"total"`
	Network bool      `json:"network"`
	ReadAt  time.Time `json:"read_at"`
	Stale   bool      `json:"stale"`
	Volumes int       `json:"volumes"`
	// xfs: the device behind the disk; new_space is what it gained, damaged
	// is "smaller" or "other_fs"; was_bytes is its size when the disk was
	// added or last grown.
	Device      string `json:"device"`
	DeviceBytes uint64 `json:"device_bytes"`
	NewSpace    uint64 `json:"new_space"`
	Damaged     string `json:"damaged"`
	WasBytes    uint64 `json:"was_bytes"`
	Formatted   bool   `json:"formatted"`
	Erroring    bool   `json:"erroring"`
}

// NodeDisk (server admin) is the node's disk from the worker's last facts.
func (h *H) NodeDisk() Endpoint {
	return Get(func(c echo.Context) (NodeDiskOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return NodeDiskOut{}, err
		}
		v, err := h.Orch.NodeDisk(rc(c), id)
		return NodeDiskOut(v), err
	})
}

// DiskPathIn is a folder on the node.
type DiskPathIn struct {
	Path string `json:"path"`
}

// FolderProbeOut is what a path looks like before it is added. A refusal is
// an answer: Code and Message say it.
type FolderProbeOut struct {
	Path       string `json:"path"`
	FSType     string `json:"fs_type"`
	Mounted    bool   `json:"mounted"`
	RootFS     bool   `json:"root_fs"`
	Network    bool   `json:"network"`
	Empty      bool   `json:"empty"`
	Free       uint64 `json:"free"`
	Total      uint64 `json:"total"`
	StampOwner string `json:"stamp_owner"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

// ProbeNodeFolder (server admin) reads a path on the node and writes nothing.
func (h *H) ProbeNodeFolder() Endpoint {
	return JSON(200, func(c echo.Context, in DiskPathIn) (FolderProbeOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return FolderProbeOut{}, err
		}
		p, err := h.Orch.ProbeNodeFolder(rc(c), middleware.Principal(c), id, in.Path)
		return FolderProbeOut(p), err
	})
}

// AddNodeFolder (server admin) queues adding the folder as the node's disk.
func (h *H) AddNodeFolder() Endpoint {
	return Job(func(c echo.Context, in DiskPathIn) (service.Job, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return service.Job{}, err
		}
		return h.Orch.AddNodeFolder(rc(c), middleware.Principal(c), id, in.Path)
	})
}

// CheckNodeDisk (server admin) queues the disk check, the way out of missing.
func (h *H) CheckNodeDisk() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return service.Job{}, err
		}
		return h.Orch.CheckNodeDisk(rc(c), middleware.Principal(c), id)
	})
}

// ---- adopt ----

// AdoptVolumeOut is a volume of an adopted env or of a forgotten disk.
type AdoptVolumeOut struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Env       string    `json:"env"`
	Where     string    `json:"where"`
	HasBackup bool      `json:"has_backup"`
	Backup    time.Time `json:"backup"`
}

func adoptVolumesOut(vs []service.AdoptVolume) []AdoptVolumeOut {
	out := make([]AdoptVolumeOut, len(vs))
	for i, v := range vs {
		out[i] = AdoptVolumeOut(v)
	}
	return out
}

// DiskAdoptPlanOut is what an adopt would do and what blocks it.
type DiskAdoptPlanOut struct {
	Node       string              `json:"node"`
	Path       string              `json:"path"`
	DiskID     string              `json:"disk_id"`
	OldNode    string              `json:"old_node"`
	Envs       []AdoptEnvOut       `json:"envs"`
	OnDisk     []AdoptVolumeOut    `json:"on_disk"`
	Elsewhere  []AdoptVolumeOut    `json:"elsewhere"`
	Unclaimed  []string            `json:"unclaimed"`
	Copies     []AdoptCopyOut      `json:"copies"`
	Hosts      []AdoptHostOut      `json:"hosts"`
	AdoptedBy  string              `json:"adopted_by"`
	Refusal    string              `json:"refusal"`
	Candidates []AdoptCandidateOut `json:"candidates"`
	ResumeDisk string              `json:"resume_disk"`
	ResumePath string              `json:"resume_path"`
}

// AdoptEnvOut is an env that moves to the node.
type AdoptEnvOut struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Tiles int    `json:"tiles"`
}

// AdoptCopyOut is a plain Docker copy that blocks the adopt.
type AdoptCopyOut struct {
	VolumeID string `json:"volume_id"`
	Slug     string `json:"slug"`
	Bound    bool   `json:"bound"`
}

// AdoptHostOut is an instance the old node hosts; it blocks the adopt.
type AdoptHostOut struct {
	Env     string `json:"env"`
	Tile    string `json:"tile"`
	Primary bool   `json:"primary"`
}

// AdoptCandidateOut is a disk row of a forgotten node.
type AdoptCandidateOut struct {
	DiskID string `json:"disk_id"`
	Node   string `json:"node"`
	Path   string `json:"path"`
	State  string `json:"state"`
}

func adoptPlanOut(p service.DiskAdoptPlan) DiskAdoptPlanOut {
	out := DiskAdoptPlanOut{Node: p.Node, Path: p.Path, DiskID: p.DiskID, OldNode: p.OldNode,
		OnDisk: adoptVolumesOut(p.OnDisk), Elsewhere: adoptVolumesOut(p.Elsewhere), Unclaimed: p.Unclaimed,
		AdoptedBy: p.AdoptedBy, Refusal: p.Refusal, ResumeDisk: p.ResumeDisk, ResumePath: p.ResumePath,
		Envs: []AdoptEnvOut{}, Copies: []AdoptCopyOut{}, Hosts: []AdoptHostOut{}, Candidates: []AdoptCandidateOut{}}
	if out.Unclaimed == nil {
		out.Unclaimed = []string{}
	}
	for _, e := range p.Envs {
		out.Envs = append(out.Envs, AdoptEnvOut(e))
	}
	for _, x := range p.Copies {
		out.Copies = append(out.Copies, AdoptCopyOut(x))
	}
	for _, x := range p.Hosts {
		out.Hosts = append(out.Hosts, AdoptHostOut(x))
	}
	for _, x := range p.Candidates {
		out.Candidates = append(out.Candidates, AdoptCandidateOut(x))
	}
	return out
}

// PlanDiskAdopt (server admin) reads what adopting the disk in ?path= would
// do; no path lists the candidates.
func (h *H) PlanDiskAdopt() Endpoint {
	return Get(func(c echo.Context) (DiskAdoptPlanOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return DiskAdoptPlanOut{}, err
		}
		p, err := h.Orch.PlanDiskAdopt(rc(c), id, c.QueryParam("path"))
		return adoptPlanOut(p), err
	}).Q("path")
}

// DiskAdoptIn is an adopt: Confirm is the node's name or the disk's id; a
// resume needs none.
type DiskAdoptIn struct {
	DiskID  string `json:"disk_id"`
	Path    string `json:"path"`
	Confirm string `json:"confirm"`
}

// AdoptDisk (server admin) makes a forgotten node's disk this node's disk.
func (h *H) AdoptDisk() Endpoint {
	return Job(func(c echo.Context, in DiskAdoptIn) (service.Job, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return service.Job{}, err
		}
		return h.Orch.AdoptDisk(rc(c), middleware.Principal(c), id, in.DiskID, in.Path, in.Confirm)
	})
}

// DiskAdoptDiscardIn removes a plain copy that blocks an adopt; Confirm is
// the volume's slug.
type DiskAdoptDiscardIn struct {
	DiskID   string `json:"disk_id"`
	Path     string `json:"path"`
	VolumeID string `json:"volume_id"`
	Confirm  string `json:"confirm"`
}

// DiscardAdoptCopy (server admin) removes a plain copy that blocks an adopt.
// The folder stays.
func (h *H) DiscardAdoptCopy() Endpoint {
	return Done(func(c echo.Context, in DiskAdoptDiscardIn) error {
		id, err := h.nodeID(c)
		if err != nil {
			return err
		}
		return h.Orch.DiscardAdoptCopy(rc(c), middleware.Principal(c), id, in.Path, in.DiskID, in.VolumeID, in.Confirm)
	})
}

// ---- forget ----

// ForgetPlanOut is what forgetting a disk would do.
type ForgetPlanOut struct {
	DiskID  string           `json:"disk_id"`
	Node    string           `json:"node"`
	Gone    bool             `json:"gone"`
	Volumes []AdoptVolumeOut `json:"volumes"`
	Refusal string           `json:"refusal"`
	// ObserveOn: logs and metrics are on this disk and a forget loses them.
	ObserveOn bool `json:"observe_on"`
}

func forgetPlanOut(p service.ForgetPlan) ForgetPlanOut {
	return ForgetPlanOut{DiskID: p.DiskID, Node: p.Node, Gone: p.Gone, Volumes: adoptVolumesOut(p.Volumes), Refusal: p.Refusal, ObserveOn: p.ObserveOn}
}

// PlanDiskForget (server admin) reads what forgetting the disk would do.
func (h *H) PlanDiskForget() Endpoint {
	return Get(func(c echo.Context) (ForgetPlanOut, error) {
		p, err := h.Orch.PlanDiskForget(rc(c), c.Param("disk"))
		return forgetPlanOut(p), err
	})
}

// ConfirmIn is the typed confirmation of a destructive verb.
type ConfirmIn struct {
	Confirm string `json:"confirm"`
}

// ForgetNodeDisk (server admin) forgets a missing or half added disk, or one
// whose node is gone; Confirm is the node's name or the disk's id. A disk of
// a forgotten node goes at once and the job is empty.
func (h *H) ForgetNodeDisk() Endpoint {
	return Job(func(c echo.Context, in ConfirmIn) (service.Job, error) {
		return h.Orch.ForgetNodeDisk(rc(c), middleware.Principal(c), c.Param("disk"), in.Confirm)
	})
}

// ---- move to disk ----

// MoveAllVolumeOut is a volume of a move-all plan.
type MoveAllVolumeOut struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Env       string `json:"env"`
	SizeBytes int64  `json:"size_bytes"`
	Finishes  bool   `json:"finishes"`
	Reason    string `json:"reason"`
}

// MoveAllGroupOut is the volumes that move in one job.
type MoveAllGroupOut struct {
	Volumes   []MoveAllVolumeOut `json:"volumes"`
	Tiles     []string           `json:"tiles"`
	SizeBytes int64              `json:"size_bytes"`
}

// MoveAllPlanOut is what moving every volume of the node to its disk does.
type MoveAllPlanOut struct {
	NodeID  string             `json:"node_id"`
	Groups  []MoveAllGroupOut  `json:"groups"`
	Skipped []MoveAllVolumeOut `json:"skipped"`
	Need    int64              `json:"need"`
	Free    uint64             `json:"free"`
	Fits    bool               `json:"fits"`
	Busy    bool               `json:"busy"`
}

func moveAllVolumesOut(vs []service.MoveAllVolume) []MoveAllVolumeOut {
	out := make([]MoveAllVolumeOut, len(vs))
	for i, v := range vs {
		out[i] = MoveAllVolumeOut(v)
	}
	return out
}

// PlanMoveAllToDisk (server admin) is the groups and the room check.
func (h *H) PlanMoveAllToDisk() Endpoint {
	return Get(func(c echo.Context) (MoveAllPlanOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return MoveAllPlanOut{}, err
		}
		p, err := h.Orch.PlanMoveAllToDisk(rc(c), middleware.Principal(c), id)
		out := MoveAllPlanOut{NodeID: p.NodeID, Need: p.Need, Free: p.Free, Fits: p.Fits, Busy: p.Busy,
			Groups: []MoveAllGroupOut{}, Skipped: moveAllVolumesOut(p.Skipped)}
		for _, g := range p.Groups {
			t := g.Tiles
			if t == nil {
				t = []string{}
			}
			out.Groups = append(out.Groups, MoveAllGroupOut{Volumes: moveAllVolumesOut(g.Volumes), Tiles: t, SizeBytes: g.SizeBytes})
		}
		return out, err
	})
}

// MoveAllOut is the jobs that were queued; Refused holds why the other
// groups were not.
type MoveAllOut struct {
	Jobs    []service.Job `json:"jobs"`
	Refused string        `json:"refused"`
}

// MoveAllToDisk (server admin) queues one job per group, smallest first. A
// group that cannot be queued is refused on its own: the others still run.
func (h *H) MoveAllToDisk() Endpoint {
	return JSON(202, func(c echo.Context, _ None) (MoveAllOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return MoveAllOut{}, err
		}
		js, err := h.Orch.MoveAllToDisk(rc(c), middleware.Principal(c), id)
		if err != nil && len(js) == 0 {
			return MoveAllOut{}, err
		}
		out := MoveAllOut{Jobs: js}
		if out.Jobs == nil {
			out.Jobs = []service.Job{}
		}
		if err != nil {
			out.Refused = err.Error()
		}
		return out, nil
	}).Idempotent()
}

// VolumeMovePlanOut is what moving one volume to the disk does.
type VolumeMovePlanOut struct {
	Slug      string   `json:"slug"`
	Env       string   `json:"env"`
	Tiles     []string `json:"tiles"`
	SizeBytes int64    `json:"size_bytes"`
	Need      int64    `json:"need"`
	Free      uint64   `json:"free"`
	Fits      bool     `json:"fits"`
	Finish    bool     `json:"finish"`
}

// PlanVolumeMove (tile.write) measures the volume and the disk.
func (h *H) PlanVolumeMove() Endpoint {
	return Get(func(c echo.Context) (VolumeMovePlanOut, error) {
		p, err := h.Orch.PlanVolumeMove(rc(c), middleware.Principal(c), c.Param("volume"))
		if p.Tiles == nil {
			p.Tiles = []string{}
		}
		return VolumeMovePlanOut(p), err
	})
}

// MoveVolumeToDisk (tile.write) queues the copy of one volume to the node's disk.
func (h *H) MoveVolumeToDisk() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		return h.Orch.MoveVolumeToDisk(rc(c), middleware.Principal(c), c.Param("volume"))
	})
}

// DiscardPlainCopy (tile.write) removes the plain Docker object of a volume
// that also has a folder; Confirm is the volume's slug. The folder stays.
func (h *H) DiscardPlainCopy() Endpoint {
	return Done(func(c echo.Context, in ConfirmIn) error {
		return h.Orch.DiscardPlainCopy(rc(c), middleware.Principal(c), c.Param("volume"), in.Confirm)
	})
}

// DiscardBoundObject (tile.write) removes the Docker object of a volume bound
// somewhere other than its folder; Confirm is the volume's slug. Both
// folders stay.
func (h *H) DiscardBoundObject() Endpoint {
	return Done(func(c echo.Context, in ConfirmIn) error {
		return h.Orch.DiscardBoundObject(rc(c), middleware.Principal(c), c.Param("volume"), in.Confirm)
	})
}

// ---- whole disks (xfs) ----

// DisksOut is the whole disks the node has, each with why it can or cannot be
// formatted.
type DisksOut struct {
	Devices []service.DiskDevice `json:"devices"`
}

// NodeDevices (server admin) lists the node's whole disks. It runs the host
// helper on the node, so it is a POST: a GET never does.
func (h *H) NodeDevices() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (DisksOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return DisksOut{}, err
		}
		d, err := h.Orch.NodeDevices(rc(c), middleware.Principal(c), id)
		return DisksOut{Devices: append([]service.DiskDevice{}, d...)}, err
	})
}

// AddDeviceIn is the disk as the list gave it, whole, so one that changed
// since is caught. Confirm is the device's name (sdb, /dev/sdb or its id).
// Force is Format anyway: only a disk that holds data needs it.
type AddDeviceIn struct {
	Device  service.DiskDevice `json:"device"`
	Confirm string             `json:"confirm"`
	Force   bool               `json:"force"`
}

// AddNodeDevice (server admin) queues formatting the picked disk xfs and
// making it the node's disk.
func (h *H) AddNodeDevice() Endpoint {
	return Job(func(c echo.Context, in AddDeviceIn) (service.Job, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return service.Job{}, err
		}
		return h.Orch.AddNodeDevice(rc(c), middleware.Principal(c), id, in.Device, in.Confirm, in.Force)
	})
}

// GrowNodeDisk (server admin) queues growing the xfs disk into the space its
// device gained.
func (h *H) GrowNodeDisk() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return service.Job{}, err
		}
		return h.Orch.GrowNodeDisk(rc(c), middleware.Principal(c), id)
	})
}

// VolumeCapIn is a volume's size cap in MB; null removes it.
type VolumeCapIn struct {
	MaxSizeMB *int64 `json:"max_size_mb"`
}

// VolumeCapOut is the cap as saved, and whether it is a limit: only a volume
// on a whole disk is held to it.
type VolumeCapOut struct {
	MaxSizeMB *int64 `json:"max_size_mb"`
	Enforced  bool   `json:"enforced"`
}

// SetVolumeCap (tile.write) sets or removes a volume's size cap. A cap under
// what the volume holds is refused and nothing changes.
func (h *H) SetVolumeCap() Endpoint {
	return JSON(200, func(c echo.Context, in VolumeCapIn) (VolumeCapOut, error) {
		id := c.Param("volume")
		if err := h.Orch.SetVolumeCap(rc(c), id, in.MaxSizeMB); err != nil {
			return VolumeCapOut{}, err
		}
		v, err := h.Orch.Volume(rc(c), id)
		if err != nil {
			return VolumeCapOut{}, err
		}
		on, err := h.Orch.CapEnforced(rc(c), v)
		return VolumeCapOut{MaxSizeMB: v.MaxSizeMB, Enforced: on}, err
	})
}

// MovePanelPlanOut is what moving the panel database onto the node disk (or
// back) does.
type MovePanelPlanOut struct {
	NodeID     string `json:"node_id"`
	DiskID     string `json:"disk_id"`
	Back       bool   `json:"back"`
	SizeBytes  int64  `json:"size_bytes"`
	DiskFree   uint64 `json:"disk_free"`
	SystemFree uint64 `json:"system_free"`
	Need       int64  `json:"need"`
	Fits       bool   `json:"fits"`
	Restarts   string `json:"restarts"`
	Warning    string `json:"warning"`
	Refusal    string `json:"refusal"`
}

// PlanMovePanelDB (server admin) measures the database and both disks; it is
// advisory, the job judges again. ?back=true plans the move back.
func (h *H) PlanMovePanelDB() Endpoint {
	return Get(func(c echo.Context) (MovePanelPlanOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return MovePanelPlanOut{}, err
		}
		p, err := h.Orch.PlanMovePanelDB(rc(c), middleware.Principal(c), id, c.QueryParam("back") == "true")
		return MovePanelPlanOut(p), err
	})
}

// MovePanelIn picks the direction.
type MovePanelIn struct {
	Back bool `json:"back"`
}

// MovePanelDB (server admin) queues the move of the panel database onto the
// node disk, or back with back=true.
func (h *H) MovePanelDB() Endpoint {
	return Job(func(c echo.Context, in MovePanelIn) (service.Job, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return service.Job{}, err
		}
		return h.Orch.MovePanelDB(rc(c), middleware.Principal(c), id, in.Back)
	})
}

// PanelDBOut is the card's panel database row.
type PanelDBOut struct {
	Applicable    bool      `json:"applicable"`
	Reason        string    `json:"reason"`
	OnDisk        bool      `json:"on_disk"`
	Moving        bool      `json:"moving"`
	DiskID        string    `json:"disk_id"`
	OldCopy       bool      `json:"old_copy"`
	OldCopyOnDisk bool      `json:"old_copy_on_disk"`
	OldCopyAt     time.Time `json:"old_copy_at"`
}

// PanelDBState (server admin) is where the panel database is.
func (h *H) PanelDBState() Endpoint {
	return Get(func(c echo.Context) (PanelDBOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return PanelDBOut{}, err
		}
		v, err := h.Orch.PanelDBState(rc(c), id)
		return PanelDBOut(v), err
	})
}

// RemovePanelDBCopy (server admin) removes the old copy a move left. It is the
// only deleter.
func (h *H) RemovePanelDBCopy() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		id, err := h.nodeID(c)
		if err != nil {
			return err
		}
		return h.Orch.RemovePanelDBOldCopy(rc(c), middleware.Principal(c), id)
	})
}
