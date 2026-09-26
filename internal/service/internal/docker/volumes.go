package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
)

// CreateVolume creates a volume carrying labels; one that exists is fine.
// NOT idempotent across differing opts: docker returns the existing volume
// unchanged, so an edit is remove-then-recreate. driver/opts "" gives local.
func (d *Client) CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error {
	all := map[string]string{LabelManaged: "true"}
	maps.Copy(all, labels)
	_, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:       name,
		Driver:     driver,
		DriverOpts: opts,
		Labels:     all,
	})
	return err
}

// RemoveVolume removes a volume; a missing one is fine. Docker refuses while
// any container, running or not, references it.
func (d *Client) RemoveVolume(ctx context.Context, name string) error {
	err := d.cli.VolumeRemove(ctx, name, false)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// InspectVolume and ListVolumes fill SizeBytes from DiskUsage (a full scan,
// click-time only) and UsedBy/HeldBy from one container list.
func (d *Client) InspectVolume(ctx context.Context, name string) (VolumeInfo, error) {
	v, err := d.cli.VolumeInspect(ctx, name)
	if err != nil {
		return VolumeInfo{}, wrap(err)
	}
	out, err := d.volumeInfos(ctx, []*volume.Volume{&v})
	if err != nil {
		return VolumeInfo{}, err
	}
	return out[0], nil
}

// ListVolumes returns the volumes carrying all of labels.
func (d *Client) ListVolumes(ctx context.Context, labels map[string]string) ([]VolumeInfo, error) {
	resp, err := d.cli.VolumeList(ctx, volume.ListOptions{Filters: labelFilter(labels)})
	if err != nil {
		return nil, err
	}
	return d.volumeInfos(ctx, resp.Volumes)
}

func (d *Client) volumeInfos(ctx context.Context, vols []*volume.Volume) ([]VolumeInfo, error) {
	du, err := d.cli.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}})
	if err != nil {
		return nil, err
	}
	size := map[string]int64{}
	for _, v := range du.Volumes {
		if v.UsageData != nil {
			size[v.Name] = v.UsageData.Size
		}
	}
	cs, err := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]VolumeInfo, 0, len(vols))
	for _, v := range vols {
		info := VolumeInfo{
			Name:       v.Name,
			Driver:     v.Driver,
			Created:    v.CreatedAt,
			Mountpoint: v.Mountpoint,
			Labels:     v.Labels,
			SizeBytes:  -1,
		}
		if s, ok := size[v.Name]; ok {
			info.SizeBytes = s
		}
		for _, c := range cs {
			for _, m := range c.Mounts {
				if m.Type != "volume" || m.Name != v.Name {
					continue
				}
				name := c.ID
				if len(c.Names) > 0 {
					name = strings.TrimPrefix(c.Names[0], "/")
				}
				if c.State == "running" {
					info.UsedBy = append(info.UsedBy, name)
				} else {
					info.HeldBy = append(info.HeldBy, name)
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// ToolImage is the throwaway container's image. A var: an air-gapped install
// points it at an image it already has.
var ToolImage = "alpine:3"

// EnsureTool pulls ToolImage if it is not on the host. Call it before
// freezing a container: a pull inside the pause window is downtime.
func (d *Client) EnsureTool(ctx context.Context) error {
	if _, err := d.cli.ImageInspect(ctx, ToolImage); err == nil {
		return nil
	}
	return d.Pull(ctx, ToolImage, "", io.Discard)
}

// TarVolume streams a gzipped tar of the whole volume into w, members
// relative to the volume root. live = the caller chose not to stop the
// writer, so "file changed as we read it" is tolerated; nothing else is.
func (d *Client) TarVolume(ctx context.Context, vol string, w io.Writer, live bool) error {
	out, wait, err := d.volumeTool(ctx, vol, true, []string{"tar", "-czf", "-", "-C", "/data", "."}, nil)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, out); err != nil {
		_ = wait() // reap the container even on a failed copy
		return fmt.Errorf("tar %s: %w", vol, err)
	}
	if err := wait(); err != nil && (!live || !isTarChanged(err)) {
		return fmt.Errorf("tar %s: %w", vol, err)
	}
	return nil
}

// isTarChanged: busybox says "short read", GNU "file changed as we read it";
// both mean the archive is intact except for that one file.
func isTarChanged(err error) bool {
	m := err.Error()
	return strings.Contains(m, "short read") || strings.Contains(m, "file changed as we read it")
}

// UntarVolume wipes the volume and extracts src into it, in one container and
// one command so a crash cannot leave it emptied with no archive in it.
// There is no way back from the wipe: the caller verifies the archive first.
// The three globs catch dotfiles; ";" so an empty volume still extracts.
func (d *Client) UntarVolume(ctx context.Context, vol string, src io.Reader) error {
	out, wait, err := d.volumeTool(
		ctx,
		vol,
		false,
		[]string{"sh", "-c", "rm -rf /data/..?* /data/.[!.]* /data/* 2>/dev/null; tar -xzf - -C /data"},
		src,
	)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, out)
	if err := wait(); err != nil {
		return fmt.Errorf("restore into %s: %w", vol, err)
	}
	return nil
}

// volumeTool mounts vol at /data (ro for reads) in a throwaway container.
func (d *Client) volumeTool(
	ctx context.Context,
	vol string,
	ro bool,
	cmd []string,
	stdin io.Reader,
) (io.Reader, func() error, error) {
	if err := d.EnsureTool(ctx); err != nil {
		return nil, nil, err
	}
	mnt := vol + ":/data"
	if ro {
		mnt += ":ro"
	}
	return d.toolContainer(ctx, cmd, mnt, stdin)
}

// toolContainer starts a throwaway container and hands back its stdout plus a
// wait reporting its exit. The caller MUST call wait on every path: it is what
// removes the container.
func (d *Client) toolContainer(
	ctx context.Context,
	cmd []string,
	bind string,
	stdin io.Reader,
) (io.Reader, func() error, error) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	created, err := d.cli.ContainerCreate(
		ctx,
		&container.Config{
			Image: ToolImage,
			// CLEARED: an image of ours has our binary as ENTRYPOINT, which
			// would turn `sh -c ...` into its arguments.
			Entrypoint:   strslice.StrSlice{},
			Cmd:          cmd,
			AttachStdin:  stdin != nil,
			OpenStdin:    stdin != nil,
			StdinOnce:    stdin != nil,
			AttachStdout: true,
			AttachStderr: true,
			Labels:       map[string]string{LabelManaged: "true"},
		},
		// AutoRemove off: the exit code has to be readable after the process
		// ends, and a self-removing container races ContainerWait.
		&container.HostConfig{Binds: []string{bind}},
		nil,
		nil,
		"stkr-vol-"+hex.EncodeToString(b),
	) // random: overlapping ops must not collide
	if err != nil {
		return nil, nil, err
	}
	remove := func() {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}
	att, err := d.cli.ContainerAttach(ctx, created.ID, container.AttachOptions{
		Stream: true,
		Stdin:  stdin != nil,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		remove()
		return nil, nil, err
	}
	// Wait BEFORE start, with NextExit: a created container is already "not
	// running", so NotRunning would answer 0 at once.
	okC, errC := d.cli.ContainerWait(context.WithoutCancel(ctx), created.ID, container.WaitConditionNextExit)
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		att.Close()
		remove()
		return nil, nil, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			_ = att.CloseWrite() // half-close: EOF on stdin, output stays up
		}()
	}
	pr, pw := io.Pipe()
	var stderr strings.Builder
	copyDone := make(chan struct{})
	go func() {
		_, err := stdcopy.StdCopy(pw, &stderr, att.Reader)
		_ = pw.CloseWithError(err)
		close(copyDone)
	}()
	wait := func() error {
		// Read side first: an abandoned stream otherwise wedges StdCopy.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-copyDone
		att.Close()
		defer remove()
		select {
		case err := <-errC:
			return err
		case st := <-okC:
			if st.Error != nil && st.Error.Message != "" {
				return fmt.Errorf("%s", st.Error.Message)
			}
			if st.StatusCode != 0 {
				return ExitError{Code: int(st.StatusCode), Stderr: strings.TrimSpace(stderr.String())}
			}
			return nil
		}
	}
	return pr, wait, nil
}
