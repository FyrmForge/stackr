package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// restore puts the install back to a panel archive (a pre-upgrade one, or
// any panel backup): the database and master key from the archive, the panel
// container on the build the archive names. The way back when an upgraded
// panel passed its gate and still turned out broken.
func restore(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stackr-install restore", flag.ContinueOnError)
	pass := fs.String("passphrase", "", "recovery passphrase (default: this box's master key)")
	image := fs.String("image", "", "panel image to run (default: the build the archive names)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: stackr-install restore [--passphrase P] [--image REF] <archive>")
	}
	dataDir := "/var/lib/stackr"
	if d := os.Getenv("STACKR_DATA_DIR"); d != "" {
		dataDir = d
	}
	if err := preflight(ctx); err != nil {
		return err
	}
	in, err := installspec.Load(dataDir)
	if err != nil {
		return fmt.Errorf("no install answers in %s (install first): %w", dataDir, err)
	}
	if *pass == "" {
		b, err := os.ReadFile(filepath.Join(dataDir, installspec.KeyFile))
		if err != nil {
			return fmt.Errorf("no --passphrase and no master key on this box: %w", err)
		}
		*pass = strings.TrimSpace(string(b))
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	stage := filepath.Join(dataDir, "restore-"+time.Now().UTC().Format("20060102T150405"))
	ver, err := unpack(f, *pass, stage)
	if err != nil {
		return fmt.Errorf("read archive (nothing changed): %w", err)
	}
	if *image == "" {
		v, err := checkVersion(ver, "")
		if err != nil {
			return fmt.Errorf("the archive names build %q; pass --image", ver)
		}
		*image = installspec.Image(v)
	}
	r := runner{out: out}
	if err := r.pull(ctx, *image); err != nil {
		return err
	}

	// Verified and staged; only now touch the running install.
	for _, role := range []string{"upgrader", "panel"} {
		ids, _ := read(ctx, "docker", "ps", "-aq", "--filter", "label="+installspec.LabelRole+"="+role)
		for _, id := range strings.Fields(ids) {
			if err := r.docker(ctx, "rm", "-f", id); err != nil {
				return err
			}
		}
	}
	db := filepath.Join(dataDir, "stackr.db")
	if err := os.Rename(db, filepath.Join(stage, "stackr.db.before-restore")); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(db + "-wal")
	_ = os.Remove(db + "-shm")
	for _, name := range []string{"stackr.db", installspec.KeyFile} {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(dataDir, name)); err != nil {
			return err
		}
	}
	if err := r.docker(ctx, installspec.Panel(*image, in).RunArgs()...); err != nil {
		return err
	}
	r.say("Restored", fs.Arg(0), "on", *image+". The replaced database is in", stage)
	return nil
}

// unpack decrypts a panel archive into dir and returns the build it names.
// Only the three names a panel archive holds are accepted.
func unpack(src io.Reader, pass, dir string) (string, error) {
	id, err := age.NewScryptIdentity(pass)
	if err != nil {
		return "", err
	}
	plain, err := age.Decrypt(src, id)
	if err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		return "", err
	}
	want := map[string]os.FileMode{"stackr.db": 0o600, installspec.KeyFile: 0o600, "VERSION": 0o644}
	var version string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		mode, ok := want[h.Name]
		if !ok || h.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("unexpected entry %q; not a panel archive", h.Name)
		}
		delete(want, h.Name)
		if h.Name == "VERSION" {
			b, err := io.ReadAll(io.LimitReader(tr, 256))
			if err != nil {
				return "", err
			}
			version = strings.TrimSpace(string(b))
			continue
		}
		w, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(w, tr)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
	}
	if len(want) > 0 {
		return "", errors.New("the archive is incomplete")
	}
	return version, nil
}
