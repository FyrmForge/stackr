package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/image"
)

// Pull pulls ref (repo@digest or repo:tag) with auth, the base64 blob the
// API's X-Registry-Auth header wants; "" = anonymous. Per call, never `docker
// login`: the daemon config is shared by every org on the node.
func (d *Client) Pull(ctx context.Context, ref, auth string, log io.Writer) error {
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: auth})
	if err != nil {
		return wrap(err)
	}
	defer func() { _ = rc.Close() }()
	return drainProgress(rc, log)
}

type progressLine struct {
	Status      string `json:"status"`
	Progress    string `json:"progress"`
	ID          string `json:"id"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// drainProgress decodes the JSON progress stream to EOF, printing "<id>:
// <status>" and skipping the per-layer progress bars. The error the STREAM
// reports is the only signal: the HTTP call succeeds even when the pull fails.
func drainProgress(r io.Reader, log io.Writer) error {
	dec := json.NewDecoder(r)
	var streamErr error
	for {
		var l progressLine
		if err := dec.Decode(&l); err != nil {
			if errors.Is(err, io.EOF) {
				return streamErr
			}
			return err
		}
		switch {
		case l.ErrorDetail != nil:
			if streamErr == nil {
				streamErr = errors.New(l.ErrorDetail.Message)
			}
		case l.Progress != "":
		case l.ID != "":
			_, _ = fmt.Fprintf(log, "%s: %s\n", l.ID, l.Status)
		case l.Status != "":
			_, _ = fmt.Fprintln(log, l.Status)
		}
	}
}

// LocalDigest returns the registry digest the local image for ref was pulled
// from, "" when it was built locally or never pulled.
func (d *Client) LocalDigest(ctx context.Context, ref string) (string, error) {
	info, err := d.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", wrap(err)
	}
	return pickDigest(ref, info.RepoDigests), nil
}

// pickDigest matches RepoDigests against ref with the tag stripped (the tag is
// after the LAST ":" only when that is after the last "/", or a registry port
// becomes the tag), falling back to any digest: the image may be tagged into
// another repo locally, and a pulled digest beats none.
func pickDigest(ref string, repoDigests []string) string {
	repo := ref
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo = ref[:i]
	}
	for _, rd := range repoDigests {
		if name, dg, ok := strings.Cut(rd, "@"); ok && name == repo {
			return dg
		}
	}
	for _, rd := range repoDigests {
		if _, dg, ok := strings.Cut(rd, "@"); ok {
			return dg
		}
	}
	return ""
}

func (d *Client) Tag(ctx context.Context, src, dst string) error {
	return wrap(d.cli.ImageTag(ctx, src, dst))
}

// RemoveImage removes ref without force, so an in-use image survives.
func (d *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := d.cli.ImageRemove(ctx, ref, image.RemoveOptions{})
	return wrap(err)
}

// ListImages returns the local images carrying all of labels.
func (d *Client) ListImages(ctx context.Context, labels map[string]string) ([]Image, error) {
	ims, err := d.cli.ImageList(ctx, image.ListOptions{Filters: labelFilter(labels)})
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(ims))
	for _, im := range ims {
		out = append(out, Image{ID: im.ID, Tags: im.RepoTags, Labels: im.Labels})
	}
	return out, nil
}

// PruneImages removes every image carrying labels unless its id or one of its
// tags is in keep. Which images to keep is the caller's; an image still used
// by a container is skipped. Returns the refs removed.
func (d *Client) PruneImages(ctx context.Context, labels map[string]string, keep []string) ([]string, error) {
	ims, err := d.ListImages(ctx, labels)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, im := range ims {
		if slices.Contains(keep, im.ID) ||
			slices.ContainsFunc(im.Tags, func(t string) bool { return slices.Contains(keep, t) }) {
			continue
		}
		refs := im.Tags // untag each; the last one deletes the image
		if len(refs) == 0 {
			refs = []string{im.ID}
		}
		for _, ref := range refs {
			switch err := d.RemoveImage(ctx, ref); {
			case err == nil:
				removed = append(removed, ref)
			case cerrdefs.IsConflict(err), errors.Is(err, ErrNotFound):
			default:
				errs = append(errs, err)
			}
		}
	}
	return removed, errors.Join(errs...)
}

// EnsureBuilder creates a buildx docker-container builder capped at memMB with
// a low cpu weight, so a build yields to running tiles. The daemon's own
// builder ignores every resource flag. Shells out: buildx has no API.
func (d *Client) EnsureBuilder(ctx context.Context, name string, memMB int) error {
	if exec.CommandContext(ctx, "docker", "buildx", "inspect", name).Run() == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "buildx", "create", "--name", name,
		"--driver", "docker-container",
		"--driver-opt", fmt.Sprintf("memory=%dm", memMB),
		"--driver-opt", "cpu-shares=256").CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildx create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Build builds dir into tag with buildx, streaming plain progress to log, and
// returns the image id. --load puts the result in the daemon.
func (d *Client) Build(
	ctx context.Context,
	builder, dir, dockerfile, tag string,
	buildArgs, labels map[string]string,
	log io.Writer,
) (string, error) {
	args := []string{"buildx", "build", "--builder", builder, "--load", "-f", dockerfile, "-t", tag, "--progress=plain"}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	cmd := exec.CommandContext(ctx, "docker", append(args, ".")...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("buildx build: %w", err)
	}
	info, err := d.cli.ImageInspect(ctx, tag)
	if err != nil {
		return "", wrap(err)
	}
	return info.ID, nil
}
