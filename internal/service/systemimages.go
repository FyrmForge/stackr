package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/move"
	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// System images: stackr's own images that nodes run (the alpine tool image,
// the pause image, the stackr image behind an ingress worker's Caddy). They
// live in the cluster registry under stackr.system/, and the controller loads
// them into every node's Docker from there (docker.Client.Pull), so no node
// reaches an outside registry. The controller publishes them at boot, which is also
// right after every upgrade (the new panel boots). Join, before any tunnel
// exists, still downloads from the public release.

// systemRepoPrefix is where system images sit in the registry.
const systemRepoPrefix = regfront.SystemOrg + "/"

// systemCopies are the outside images copied in: upstream ref to repo and tag.
// The tool image and the pause image refs nodes use (docker.ToolImage,
// tile.PauseImage) are the localhost names of these.
var systemCopies = []struct{ upstream, repo, tag string }{
	{"alpine:3", systemRepoPrefix + "alpine", "3"},
	{tile.PauseUpstream, systemRepoPrefix + "pause", "3.10"},
}

// stackrRepo is the stackr image's repo; the tag is the running version.
const stackrRepo = systemRepoPrefix + "stackr"

// stackrDigests is the digest pushLocal recorded for each published stackr
// tag. Nodes pull the helper and the ingress proxy by it, so a tag that
// someone overwrote in the registry is never what runs as root.
var stackrDigests sync.Map // tag -> digest

// stackrTagRef is the registry ref the controller publishes the stackr image
// under, given the ref the controller's own proxy container runs.
func stackrTagRef(image string) string {
	tag := "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		tag = image[i+1:]
	}
	return "localhost:" + installspec.RegistryPort + "/" + stackrRepo + ":" + tag
}

// stackrSystemRef is the ref every node pulls the stackr image by, given the
// ref the controller's own proxy container runs: the digest the boot publish
// recorded. With no digest there is no ref: a tag could be one someone
// overwrote, and this image runs as root.
func stackrSystemRef(image string) (string, error) {
	ref := stackrTagRef(image)
	i := strings.LastIndex(ref, ":")
	if dg, ok := stackrDigests.Load(ref[i+1:]); ok {
		return ref[:i] + "@" + dg.(string), nil
	}
	return "", fmt.Errorf("the stackr image %s is not published in the cluster registry", ref[i+1:])
}

// systemPublisher is the publish step with its effects as funcs, so a test
// swaps them. platforms is clusterPlatforms; copy is imgcopy.Copy into the
// local registry; pushLocal tags and pushes an image from the controller's
// own Docker.
type systemPublisher struct {
	platforms   func(context.Context) []string
	copy        func(ctx context.Context, src, dst string, platforms []string) (imgcopy.Result, error)
	pushLocal   func(ctx context.Context, src, dst string) (imgcopy.Result, error)
	record      func(context.Context, store.RegistryImage) error
	stackrImage func(context.Context) (string, error)
	published   func() // after the stackr image is in; may be nil
}

// publish copies every system image in. One failing does not stop the rest;
// the error joins what failed.
func (p systemPublisher) publish(ctx context.Context) error {
	// The cluster's CPU types; a controller alone (or G2's not yet there) has its own.
	plat := p.platforms(ctx)
	if len(plat) == 0 {
		plat = []string{runtime.GOARCH}
	}
	var errList []error
	add := func(src, repo, tag string, res imgcopy.Result, err error) {
		if err == nil {
			err = p.record(ctx, store.RegistryImage{
				Repo: repo, Tag: tag, Digest: res.Digest,
				Platforms: registry.JoinPlatforms(res.Platforms), Source: registry.SourceSystem,
				UpstreamRef: src, Size: res.Size, CreatedBy: "stackr",
			})
		}
		if err != nil {
			errList = append(errList, fmt.Errorf("%s: %w", src, err))
		}
	}
	local := func(repo, tag string) string { return "localhost:" + installspec.RegistryPort + "/" + repo + ":" + tag }
	// First: it is local and quick, and ingress workers wait on it.
	// Not on any outside registry yet, only in the controller's Docker.
	// ponytail: one CPU type (the controller's) until ghcr carries a
	// multi-arch image; then this is one more entry of systemCopies.
	if img, err := p.stackrImage(ctx); err != nil {
		errList = append(errList, fmt.Errorf("stackr image: %w", err))
	} else {
		dst := stackrTagRef(img)
		tag := dst[strings.LastIndex(dst, ":")+1:]
		res, err := p.pushLocal(ctx, img, dst)
		if err == nil && res.Digest == "" {
			err = errors.New("the push recorded no digest")
		}
		if err == nil {
			stackrDigests.Store(tag, res.Digest)
		}
		add(img, stackrRepo, tag, res, err)
		if err == nil && p.published != nil {
			p.published()
		}
	}
	for _, c := range systemCopies {
		res, err := p.copy(ctx, c.upstream, local(c.repo, c.tag), plat)
		add(c.upstream, c.repo, c.tag, res, err)
	}
	return errors.Join(errList...)
}

// controllerStackrImage is the image the controller's own proxy container
// runs: the running release's stackr image, which an upgrade swaps in.
func (o *Orchestrator) controllerStackrImage(ctx context.Context) (string, error) {
	ctx, err := o.withController(ctx)
	if err != nil {
		return "", err
	}
	d, err := o.docker.Inspect(ctx, installspec.ProxyName)
	if err != nil {
		return "", fmt.Errorf("controller proxy: %w", err)
	}
	return d.Image, nil
}

func (o *Orchestrator) systemPublisher() systemPublisher {
	return systemPublisher{
		platforms: o.clusterPlatforms,
		copy: func(ctx context.Context, src, dst string, platforms []string) (imgcopy.Result, error) {
			leave, err := o.regs.Hold(ctx)
			if err != nil {
				return imgcopy.Result{}, err
			}
			defer leave()
			return imgcopy.Copy(ctx, src, dst, nil, o.regs.Transport(), platforms)
		},
		pushLocal: func(ctx context.Context, src, dst string) (imgcopy.Result, error) {
			leave, err := o.regs.Hold(ctx)
			if err != nil {
				return imgcopy.Result{}, err
			}
			defer leave()
			cctx, err := o.withController(ctx)
			if err != nil {
				return imgcopy.Result{}, err
			}
			if err := o.docker.Tag(cctx, src, dst); err != nil {
				return imgcopy.Result{}, fmt.Errorf("tag: %w", err)
			}
			return o.pushLocal(cctx, dst)
		},
		record:      o.recordImage,
		stackrImage: o.controllerStackrImage,
		published:   o.markStackrPublished,
	}
}

func (o *Orchestrator) markStackrPublished() {
	if o.stackrPublished != nil {
		o.stackrOnce.Do(func() { close(o.stackrPublished) })
	}
}

// waitStackrPublished holds a node's pull of the stackr image until the boot
// publish put the running version in the registry: right after an upgrade
// the new tag is not there yet.
func (o *Orchestrator) waitStackrPublished(ctx context.Context) error {
	if o.stackrPublished == nil {
		return nil
	}
	select {
	case <-o.stackrPublished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// publishSystemImages runs at boot: the registry may still be coming up, so a
// few tries. Ingress workers wait on the stackr image, so a success re-syncs
// the proxy to bring any that failed to pull it.
func (o *Orchestrator) publishSystemImages(ctx context.Context) {
	defer o.markStackrPublished() // given up: a pull says what is missing
	// The workers roll once the image they pull is in, and once only: a
	// retry of the other copies publishes the stackr image again.
	var rolled sync.Once
	var err error
	for i := range 5 {
		p := o.systemPublisher()
		p.published = func() {
			o.markStackrPublished()
			rolled.Do(func() { o.rolloutAfterBoot(ctx) })
		}
		if err = p.publish(ctx); err == nil {
			if err := o.SyncProxy(ctx); err != nil {
				slog.Warn("system images: proxy sync", "err", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(i+1) * 10 * time.Second):
		}
	}
	slog.Warn("system images: not all published", "err", err)
}

// stackrSpares is how many stackr image versions besides the running one the
// registry sweep keeps, to roll an upgrade back.
const stackrSpares = 2

// systemInUse is the registry sweep's answer for stackr.system repos, which it
// asks with no newest-N kept: the tool, pause and rsync images keep the tags
// the code runs; the stackr image keeps the running controller's tag and the
// newest stackrSpares others (Clean asks newest first, so the first others
// asked are the newest); any other system repo is kept whole. Not safe for
// concurrent use: one sweep, one value.
func (o *Orchestrator) systemInUse() registry.InUse {
	pinned := map[string]string{} // repo -> tag the code runs
	for _, ref := range []string{docker.ToolImage, tile.PauseImage, move.RsyncImage} {
		path := strings.TrimPrefix(ref, "localhost:"+installspec.RegistryPort+"/")
		i := strings.LastIndex(path, ":")
		pinned[path[:i]] = path[i+1:]
	}
	var current string
	spares := map[string]bool{}
	return func(ctx context.Context, repo, digest string) (bool, error) {
		tag, ok := pinned[repo]
		switch {
		case ok:
			d, err := o.regs.Resolve(ctx, repo, tag)
			return d == digest, err
		case repo != stackrRepo:
			return true, nil
		}
		if current == "" {
			img, err := o.controllerStackrImage(ctx)
			if err != nil {
				return true, err
			}
			ref := stackrTagRef(img)
			if current, err = o.regs.Resolve(ctx, stackrRepo, ref[strings.LastIndex(ref, ":")+1:]); err != nil {
				return true, err
			}
		}
		if digest == current || spares[digest] {
			return true, nil
		}
		if len(spares) < stackrSpares {
			spares[digest] = true
			return true, nil
		}
		return false, nil
	}
}
