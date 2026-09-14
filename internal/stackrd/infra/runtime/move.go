package runtime

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
)

// Moving a volume between nodes.
//
// Docker has no move-volume API and never will: a local volume is a
// directory on one host's disk. The move is the two-pass copy VM live
// migration uses, run between the two nodes' agents over the stkr overlay
// with rsync (docs/plans/30-docker-swarm.md, "Moving a volume after the
// fact"):
//
//  1. the tile keeps running, rsync the volume to a fresh volume on the
//     target, dirty, and repeatable ahead of time
//  2. stop the tile, rsync again: only the delta, so seconds
//  3. flip the placement constraint and start on the target
//  4. remove the source volume only once the tile is running clean
//
// Step 4's ordering is not optional. Deleting the source before the target
// is verified and running is how the volume is lost.
//
// Neither agent can bind-mount a volume into itself at runtime, so each side
// runs rsync in a throwaway container with the volume mounted, on the stkr
// overlay: the receiver as an rsync daemon under a network alias, the sender
// pushing into it by that alias.

// MoveImage is what both sides of a move run. It has to carry rsync, and it
// has to already be on both nodes: a move is not the moment to discover a
// worker has no internet. The agent's own image is both, every node is
// already running it, and it ships rsync for exactly this
// (docs/plans/31-node-agent-open-questions.md, transport). Set by the panel
// at boot from the image its own agent service runs.
var MoveImage = "alpine:3"

// moveReceiverName is the container the receiving side runs.
func moveReceiverName(moveID string) string { return "stkr-move-" + moveID }

// rsyncConf is the receiver's daemon config. Written through the command line
// rather than a file because the container has no config to bind-mount into
// it. The module is read-write by design, it is the destination, and the
// daemon is only reachable on the stkr overlay, which no tenant network
// touches.
const rsyncConf = `[vol]
path = /data
read only = false
uid = 0
gid = 0
use chroot = false
max connections = 1
`

// StartMoveReceiver opens the receiving side of a move: a container with vol
// mounted and an rsync daemon in it, answering to the move's alias on the
// stkr overlay. Idempotent, a retried call finds the container already
// there and leaves it running.
func (r *Runtime) StartMoveReceiver(ctx context.Context, moveID, vol string) error {
	name := moveReceiverName(moveID)
	if _, err := r.cli.ContainerInspect(ctx, name); err == nil {
		return nil
	}
	// The volume has to exist before it can be mounted, and on the target
	// node it never does, this is the fresh volume the copy lands in.
	if err := r.CreateVolume(ctx, vol); err != nil {
		return err
	}
	created, err := r.cli.ContainerCreate(ctx,
		&container.Config{
			Image: MoveImage,
			// Cleared: MoveImage is the agent's own image and its ENTRYPOINT
			// is the stackrd binary (see toolContainer).
			Entrypoint: strslice.StrSlice{},
			// The config goes in base64. Interpolating it as a quoted shell
			// string does not survive: Go's %q writes the newlines as \n,
			// printf '%%s' hands them to the file literally, and rsync reads
			// a one-line config it cannot parse and exits immediately, the
			// sender then has nothing to connect to.
			Cmd: []string{"sh", "-c",
				fmt.Sprintf("echo %s | base64 -d > /etc/rsyncd.conf; "+
					"exec rsync --daemon --no-detach --port %d --config /etc/rsyncd.conf",
					base64.StdEncoding.EncodeToString([]byte(rsyncConf)), MovePort)},
			Labels: map[string]string{LabelManaged: "true", labelMove: moveID},
		},
		&container.HostConfig{Binds: []string{vol + ":/data"}},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			NetworkName: {Aliases: []string{MoveAliasPrefix + moveID}},
		}},
		nil, name)
	if err != nil {
		return err
	}
	return r.cli.ContainerStart(ctx, created.ID, container.StartOptions{})
}

// StopMoveReceiver removes the receiving container. The volume it wrote
// stays: it is the point of the exercise.
func (r *Runtime) StopMoveReceiver(ctx context.Context, moveID string) error {
	err := r.cli.ContainerRemove(ctx, moveReceiverName(moveID), container.RemoveOptions{Force: true})
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// SweepMoveReceivers removes both sides of any move left behind by a process
// that died mid-copy. Called at boot: a stray receiver holds a volume open
// and answers to an alias a later move would reuse, and a stray sender is an
// rsync still writing into it.
func (r *Runtime) SweepMoveReceivers(ctx context.Context) {
	cs, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelMove)),
	})
	if err != nil {
		return
	}
	for _, c := range cs {
		_ = r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
	}
}

// RunMoveSend runs one rsync pass from vol into the receiver answering to
// target, calling progress with rsync's own byte counters as it goes.
//
// --delete is deliberate: the second pass has to remove what the tile
// deleted between the passes, or the target ends up with files the source no
// longer has. It only ever deletes inside the fresh target volume.
func (r *Runtime) RunMoveSend(ctx context.Context, moveID, vol, target string, progress func(done, total int64)) error {
	cmd := []string{"sh", "-c", fmt.Sprintf(
		"exec rsync -a --delete --info=progress2 --no-inc-recursive /data/ rsync://%s:%d/vol/",
		target, MovePort)}

	out, wait, err := r.toolContainer(ctx, toolOpts{
		image:   MoveImage,
		cmd:     cmd,
		binds:   []string{vol + ":/data:ro"},
		network: NetworkName,
		name:    "stkr-movesend-" + randomSuffix(),
		// Labelled so SweepMoveReceivers reaps it too: a panel or agent that
		// dies mid-send otherwise leaves an rsync running against a volume
		// with nothing left to stop it.
		labels: map[string]string{labelMove: moveID},
	})
	if err != nil {
		return err
	}
	// --info=progress2 rewrites one line with \r, so scan on that too.
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(scanProgressLines)
	for sc.Scan() {
		if done, total, ok := parseRsyncProgress(sc.Text()); ok && progress != nil {
			progress(done, total)
		}
	}
	_, _ = io.Copy(io.Discard, out)
	return wait()
}

// scanProgressLines splits on \n and \r, because rsync's progress meter
// redraws with a carriage return and never sends a newline until it is done.
func scanProgressLines(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parseRsyncProgress reads "<bytes> <pct>% <rate> <elapsed> (xfr#.., to-chk=A/B)"
// and turns it into bytes done and an estimated total.
//
// rsync reports a percentage, not a total, so the total is derived from it.
// --no-inc-recursive is what makes that honest: it builds the whole file list
// before transferring, so the percentage is against the real total from the
// first line rather than against however much rsync has discovered so far.
func parseRsyncProgress(line string) (done, total int64, ok bool) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 2 || !strings.HasSuffix(f[1], "%") {
		return 0, 0, false
	}
	done, err := strconv.ParseInt(strings.ReplaceAll(f[0], ",", ""), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(f[1], "%"), 64)
	if err != nil || pct <= 0 {
		// 0% with bytes moved is the normal first line; report what is known
		// and let the caller show bytes without a bar.
		return done, 0, true
	}
	return done, int64(float64(done) / (pct / 100)), true
}

// VolumeSize is the volume's size on disk, from du on the node holding it.
// The Move modal asks for this before it offers to copy anything, so the
// estimate it shows is measured rather than guessed.
func (r *Runtime) VolumeSize(ctx context.Context, vol string) (int64, error) {
	out, err := r.volumeToolRun(ctx, vol, true, []string{"du", "-sb", "/data"}, nil)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return 0, fmt.Errorf("du returned nothing for %s", vol)
	}
	return strconv.ParseInt(f[0], 10, 64)
}
