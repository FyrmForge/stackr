// Package agent is the node agent: one task per swarm node, proxying the
// handful of docker calls Swarm's control plane does not forward.
//
// Swarm speaks in services and tasks. Exec, stats, pause, inspect and
// anything that mounts a volume are local daemon calls on the node holding
// the container, and Swarm forwards none of them, k8s has the kubelet for
// this, Swarm has nothing (docs/plans/30-docker-swarm.md, step 7).
//
// The shape, decided in docs/plans/31-node-agent-open-questions.md:
//
//   - The agent is a narrow proxy to its own node's socket: thin handlers
//     over the same infra/runtime methods, by name, with the panel's
//     arguments. The daemon API itself never goes on the wire, a full socket
//     proxy would be root on the node for anyone holding the key.
//   - Socket local, agent remote. On one node nothing new runs: the panel
//     keeps using runtime.Runtime directly. See node.go.
//   - HTTP/1.1 plus JSON for calls, websocket for the two streams (exec TTY
//     and logs). Tar and untar are request and response bodies.
//   - The agent listens on its overlay address only. The panel finds it by
//     reading the agent task's overlay IP out of swarm task state, so there
//     is nothing to configure on either side.
//
// This file is the contract. Changes to it are additive only and unknown
// fields are ignored, so a panel and an agent one version apart still talk.
package agent

import (
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

const (
	// ServiceName is the global swarm service every node runs a task of.
	ServiceName = "stkr-agent"

	// Port is the agent's listener. Fixed, because the panel finds the agent
	// by node and there is nothing to look a port up in.
	Port = 8090

	// SecretName is the swarm secret holding the shared runtime key, mounted
	// into the agent and the panel at SecretPath. One `docker secret create`;
	// a global service has one spec, so the key cannot be per node.
	SecretName = "stkr-agent-key"
	SecretPath = "/run/secrets/agent_key" //nolint:gosec // a mount path, not a credential

	// VersionHeader rides on every call. HTTP has no "connect", so this is
	// where plan 30's version handshake lives.
	VersionHeader = "X-Stackr-Agent-Version"

	// LabelAgent marks the agent's own service and its throwaway containers.
	LabelAgent = "stackr.agent"
)

// Paths under /v1. One prefix, one file, so adding a call is one line in each
// of client.go and server.go.
const (
	PathInfo    = "/v1/info"
	PathInspect = "/v1/inspect"
	PathStats   = "/v1/stats"
	PathHealth  = "/v1/health"
	PathPause   = "/v1/pause"
	PathPrune   = "/v1/prune"
	PathVolumes = "/v1/volumes"

	// The containers admin page, which lists and drives containers on every
	// node, not just the manager's.
	PathContainers      = "/v1/containers"
	PathContainerAction = "/v1/container/action"
	PathContainerSystem = "/v1/container/system"

	PathVolumeList   = "/v1/volume/list"
	PathVolumeDelete = "/v1/volume/delete"
	PathVolumeSize   = "/v1/volume/size"
	PathVolumeRead   = "/v1/volume/read"
	PathVolumeWrite  = "/v1/volume/write"
	PathVolumeTar    = "/v1/volume/tar"
	PathVolumeUntar  = "/v1/volume/untar"

	// The volume itself, not a file in it: PathVolumeDelete above removes
	// one entry inside a volume, these two make and destroy the volume. A
	// volume is a directory on one host's disk, so both have to run on the
	// node that owns it, doing them on the manager was silently deleting
	// the manager's volume while the operator looked at a worker's list.
	PathVolumeCreate = "/v1/volume/create"
	PathVolumeRemove = "/v1/volume/remove"

	PathExec      = "/v1/exec"
	PathExecRun   = "/v1/exec/run"
	PathExecShell = "/v1/exec/shell"
	PathLogs      = "/v1/logs"

	// A move is two calls, one to each agent: the target opens a receiver,
	// the source pushes into it and streams progress back.
	PathMoveReceive = "/v1/move/receive"
	PathMoveSend    = "/v1/move/send"
	PathMoveClose   = "/v1/move/close"
)

// ContainerReq names a container. The panel resolves a tile to a task and a
// node first; by the time it gets here the node is already decided.
type ContainerReq struct {
	Container string `json:"container"`
}

// PauseReq pauses or unpauses one container.
type PauseReq struct {
	Container string `json:"container"`
	Unpause   bool   `json:"unpause"`
}

// ContainerActionReq starts, stops or removes one container.
type ContainerActionReq struct {
	Container string `json:"container"`
	// Action is "start", "stop" or "remove".
	Action string `json:"action"`
}

// BoolResp is a yes/no answer.
type BoolResp struct {
	OK bool `json:"ok"`
}

// VolumeReq names a volume, and for the listing a directory inside it.
type VolumeReq struct {
	Volume string `json:"volume"`
	Dir    string `json:"dir,omitempty"`
	File   string `json:"file,omitempty"`
	// Driver and Opts are the create call's only: a storage sub-path is an
	// nfs, cifs or bind mount, which is driver opts on the volume. Empty
	// Driver means the plain local volume an older panel asked for.
	Driver string            `json:"driver,omitempty"`
	Opts   map[string]string `json:"opts,omitempty"`
}

// ExecShellReq runs one shell command in a container and waits for it. The
// scheduled-job "exec" mode is the caller; the timeout kill it does on a
// cancelled context has to happen next to the container, which is why this is
// its own call rather than two of PathExecRun.
type ExecShellReq struct {
	Container string `json:"container"`
	Command   string `json:"command"`
}

// ExecShellResp carries the combined output and the command's own failure.
// The error is a field, not a status code: the output is wanted either way
// and a failed run records both.
type ExecShellResp struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// InfoResp is the node's own description for the server page. HostInfo is the
// docker daemon's view; Distro is the one thing only the agent can see, since
// the panel's own container has no idea what the host runs.
type InfoResp struct {
	Host   runtime.HostInfo `json:"host"`
	Distro string           `json:"distro"`
}

// SampleReq is what an agent posts to the panel on its ticker: one host
// metrics sample from the node's own /proc. The panel stores it under the
// server row's ref, so a worker's graphs are the same graphs as the
// manager's.
type SampleReq struct {
	NodeID   string    `json:"node_id"`
	TS       time.Time `json:"ts"`
	CPUPct   float64   `json:"cpu_pct"`
	MemBytes int64     `json:"mem_bytes"`
	RxBps    float64   `json:"rx_bps"`
	TxBps    float64   `json:"tx_bps"`
	DiskUsed int64     `json:"disk_used"`
}

// MoveReceiveReq opens the receiving side of a volume move: a container on
// the stkr overlay with the target volume mounted and an rsync daemon in it,
// answering to MoveAlias+ID.
type MoveReceiveReq struct {
	MoveID string `json:"move_id"`
	Volume string `json:"volume"`
}

// MoveSendReq runs one rsync pass from the source volume into the receiver
// opened by MoveReceiveReq. Two passes make a move: the first with the tile
// running, the second after it stops, carrying only the delta
// (docs/plans/30-docker-swarm.md, "Moving a volume after the fact").
type MoveSendReq struct {
	MoveID string `json:"move_id"`
	Volume string `json:"volume"`
	// Target is the receiving agent's alias on the overlay.
	Target string `json:"target"`
}

// MoveProgress is one line of the send stream: rsync's own byte counters, so
// the modal's bar and ETA are real numbers and not a guess.
type MoveProgress struct {
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

// SizeResp is a volume's size in bytes, from du on the node that holds it.
type SizeResp struct {
	Bytes int64 `json:"bytes"`
}

// PruneResp is the human-readable output of an image and container prune.
type PruneResp struct {
	Output string `json:"output"`
}

// ErrResp is the body of any non-2xx reply.
type ErrResp struct {
	Error string `json:"error"`
}

// ExecErrorTrailer carries an exec's failure after its output.
//
// A dump is a stream: by the time the command fails the body has started and
// there is no status code left to say so with. A trailer is HTTP's answer to
// exactly that, and it has to be one, a truncated database dump that reports
// success is a backup nobody can restore.
const ExecErrorTrailer = "X-Stackr-Exec-Error"

// StreamErrorTrailer is ExecErrorTrailer for the volume streams: a tar of a
// volume and a read of one file off it. Same problem, same answer. Without it
// a tar that died halfway ends its chunked body normally, the client's io.Copy
// returns nil, and the backup is recorded green with half an archive in it.
const StreamErrorTrailer = "X-Stackr-Stream-Error"
