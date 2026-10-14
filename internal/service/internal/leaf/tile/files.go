package tile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	// LabelFiles marks a tile's config file volumes; its value is the tile id.
	LabelFiles = "stackr.files.tile"
	// FilesDone is the last member of a files archive: a volume without it
	// was cut short and is written again.
	FilesDone = ".stackr-done"
	// minFilesDocker is the first Docker whose mounts take a volume subpath.
	minFilesDocker = 26
)

// WriteFiles makes t's config file volume for key from tgz (a gzipped tar
// whose last member is FilesDone) on the env's node and returns its name.
// The name is the key's, so the same key finds the volume written and
// writes nothing. The key holds no secret; tgz may.
func (l *Leaf) WriteFiles(ctx context.Context, t store.Tile, key string, tgz []byte) (string, error) {
	ctx, err := l.at(ctx, t.EnvironmentID)
	if err != nil {
		return "", err
	}
	hi, err := l.docker.HostInfo(ctx)
	if err != nil {
		return "", err
	}
	if hi.Version != "" {
		major, _, _ := strings.Cut(hi.Version, ".")
		if n, err := strconv.Atoi(major); err != nil || n < minFilesDocker {
			return "", errs.Conflictf(
				"files: needs Docker %d or newer on this env's node (it runs %s)", minFilesDocker, hi.Version)
		}
	}
	sum := sha256.Sum256([]byte(key))
	name := "stackr-files-" + t.ID[:min(8, len(t.ID))] + "-" + hex.EncodeToString(sum[:])[:12]
	vols, err := l.docker.ListVolumes(ctx, map[string]string{LabelFiles: t.ID})
	if err != nil {
		return "", err
	}
	for _, v := range vols {
		if v.Name != name {
			continue
		}
		// ponytail: tars the whole volume to find the marker; fine for
		// config files, add a stat tool container if they get big.
		var buf bytes.Buffer
		if err := l.docker.TarVolume(ctx, name, &buf, false); err != nil {
			return "", err
		}
		if hasMember(buf.Bytes(), FilesDone) {
			return name, nil
		}
	}
	if err := l.docker.CreateVolume(ctx, name, "", nil, map[string]string{LabelFiles: t.ID}); err != nil {
		return "", err
	}
	if err := l.docker.UntarVolume(ctx, name, bytes.NewReader(tgz)); err != nil {
		return "", err
	}
	return name, nil
}

// hasMember reports whether the gzipped tar has a member named name at its root.
func hasMember(tgz []byte, name string) bool {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return false
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			return false
		}
		if strings.TrimPrefix(h.Name, "./") == name {
			return true
		}
	}
}

// PruneFiles removes t's config file volumes but keep ("" = all). A volume a
// container still holds is left: Docker refuses, and a later deploy retries.
func (l *Leaf) PruneFiles(ctx context.Context, t store.Tile, keep string) error {
	ctx, err := l.at(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	vols, err := l.docker.ListVolumes(ctx, map[string]string{LabelFiles: t.ID})
	if err != nil {
		return err
	}
	var errList []error
	for _, v := range vols {
		if v.Name == keep {
			continue
		}
		if err := l.docker.RemoveVolume(ctx, v.Name); err != nil && !docker.IsVolumeInUse(err) {
			errList = append(errList, fmt.Errorf("remove %s: %w", v.Name, err))
		}
	}
	return errors.Join(errList...)
}
