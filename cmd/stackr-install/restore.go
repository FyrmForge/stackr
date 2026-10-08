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
	"os/exec"
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
	replaced := filepath.Join(dataDir, "stackr.db.before-restore-"+filepath.Base(stage)[len("restore-"):])
	if err := os.Rename(db, replaced); err != nil &&
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
	// Certificates are re-issuable (rate limits aside), the database is not:
	// a proxy volume that fails to restore warns and the restore goes on.
	if err := restoreCaddy(ctx, r, *image, stage); err != nil {
		r.say("  warning: proxy volume not restored, certificates will be re-issued:", err)
	}
	if err := r.docker(ctx, installspec.Panel(*image, in).RunArgs()...); err != nil {
		return err
	}
	if err := dropStage(stage); err != nil {
		r.say("  warning: delete", stage, "by hand, it holds unencrypted certificate keys:", err)
	}
	r.say("Restored", fs.Arg(0), "on", *image+". The replaced database is", replaced)
	return nil
}

// restoreCaddy puts the staged proxy volume (certificates, keys, ACME
// accounts) back before the proxy serves anything: stop it, wipe and untar
// into stackr-caddy, start it. An archive without one is a no-op. The panel
// image does the untar: it is already here and ships tar and sh.
func restoreCaddy(ctx context.Context, r runner, image, stage string) error {
	tarball := filepath.Join(stage, caddyMember)
	f, err := os.Open(tarball)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if r.dry || r.exists(ctx, "container", installspec.ProxyName) {
		if err := r.docker(ctx, "stop", installspec.ProxyName); err != nil {
			return err
		}
		defer func() {
			if serr := r.docker(ctx, "start", installspec.ProxyName); serr != nil {
				r.say("  warning: start the proxy by hand:", serr)
			}
		}()
	}
	args := []string{
		"run", "--rm", "-i", "--entrypoint", "sh", "-v", installspec.ProxyVolume + ":/data", image,
		"-c", untarScript("/data"),
	}
	if r.dry {
		r.say("  would run: docker", quote(args))
		return nil
	}
	r.say("  docker run (untar into", installspec.ProxyVolume+")")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, r.out, r.out
	return cmd.Run()
}

// untarScript keeps a copy of dir's tree, wipes dir and untars stdin into
// it; when the untar fails the copy goes back, so a bad tarball never costs
// the certificates.
func untarScript(dir string) string {
	wipe := "rm -rf " + dir + "/..?* " + dir + "/.[!.]* " + dir + "/* 2>/dev/null"
	return "prev=$(mktemp -d) && cp -a " + dir + "/. \"$prev\"/ || exit 1\n" +
		wipe + "\n" +
		"if ! tar -xzf - -C " + dir + "; then\n" +
		"  " + wipe + "\n" +
		"  cp -a \"$prev\"/. " + dir + "/\n" +
		"  echo 'untar failed; the previous tree is back' >&2\n" +
		"  exit 1\n" +
		"fi\n"
}

// dropStage removes the staging dir: it holds the unencrypted proxy volume,
// certificate keys included.
func dropStage(stage string) error { return os.RemoveAll(stage) }

// caddyMember is the proxy volume's tar inside a panel archive.
const caddyMember = "caddy.tar.gz"

// unpack decrypts a panel archive into dir and returns the build it names.
// Only the names a panel archive holds are accepted; the proxy volume's tar
// is optional (older archives have none).
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
	optional := map[string]os.FileMode{caddyMember: 0o600}
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
		if !ok {
			mode, ok = optional[h.Name]
		}
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
