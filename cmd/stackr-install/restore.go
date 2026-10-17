package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"golang.org/x/term"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// restore puts the install back to a panel archive (a pre-upgrade one, or
// any panel backup): the database and master key from the archive, the panel
// container on the build the archive names. The way back when an upgraded
// panel passed its gate and still turned out broken.
func restore(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stackr-install restore", flag.ContinueOnError)
	passFlag := fs.String("passphrase", "", "recovery passphrase; prefer "+passphraseEnv+" or --passphrase-stdin, a flag shows in ps")
	passStdin := fs.Bool("passphrase-stdin", false, "read the recovery passphrase from the first line of stdin")
	image := fs.String("image", "", "panel image to run (default: the build the archive names)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: stackr-install restore [--passphrase-stdin] [--image REF] <archive | s3://bucket/key>")
	}
	pass, err := passphrase(*passFlag, os.Getenv(passphraseEnv), *passStdin, os.Stdin)
	if err != nil {
		return err
	}
	dataDir := "/var/lib/stackr"
	if d := os.Getenv("STACKR_DATA_DIR"); d != "" {
		dataDir = d
	}
	if err := preflight(ctx, runner{out: out}, false); err != nil {
		return err
	}
	in, err := installspec.Load(dataDir)
	if err != nil {
		return fmt.Errorf("no install answers in %s (install first): %w", dataDir, err)
	}
	fromKey := pass == ""
	if fromKey {
		b, err := os.ReadFile(filepath.Join(dataDir, installspec.KeyFile))
		if err != nil {
			return fmt.Errorf("no passphrase and no master key on this box: %w", err)
		}
		pass = strings.TrimSpace(string(b))
	}
	src := fs.Arg(0)
	if strings.HasPrefix(src, "s3://") {
		tmp, key, err := fetchS3(ctx, src, dataDir, os.Getenv)
		if err != nil {
			return fmt.Errorf("fetch the archive (nothing changed): %w", err)
		}
		defer func() { _ = os.Remove(tmp) }()
		_, _ = fmt.Fprintln(out, "Fetched", key)
		src = tmp
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	stage := filepath.Join(dataDir, "restore-"+time.Now().UTC().Format("20060102T150405"))
	ver, err := unpack(f, pass, stage)
	if err != nil && fromKey && term.IsTerminal(int(os.Stdin.Fd())) {
		// On a new box the master key is the new install's, not the
		// archive's: ask for the recovery passphrase.
		_, _ = fmt.Fprint(out, "This box's master key does not open the archive. Recovery passphrase: ")
		b, perr := term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(out)
		if perr != nil {
			return perr
		}
		if _, err = f.Seek(0, io.SeekStart); err == nil {
			ver, err = unpack(f, strings.TrimSpace(string(b)), stage)
		}
	}
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
	for _, m := range installspec.PanelState {
		if !m.OnDisk {
			continue
		}
		src := filepath.Join(stage, m.Name)
		if _, err := os.Stat(src); m.Optional && errors.Is(err, os.ErrNotExist) {
			// A previous controller cert left from before would co-sign
			// recovery answers the archived state had stopped; a previous
			// root left from before would start a rollover it never had.
			switch m.Name {
			case installspec.ControllerPrevKeyFile, installspec.ControllerPrevCertFile,
				installspec.ClusterCAPrevCertFile, installspec.ClusterCACrossCertFile:
				if err := os.Remove(filepath.Join(dataDir, m.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dataDir, m.Name)), 0o700); err != nil {
			return err
		}
		if err := os.Rename(src, filepath.Join(dataDir, m.Name)); err != nil {
			return err
		}
	}
	// Certificates are re-issuable (rate limits aside), the database is not:
	// a proxy volume that fails to restore warns and the restore goes on.
	if err := restoreCaddy(ctx, r, *image, stage); err != nil {
		r.say("  warning: proxy volume not restored, certificates will be re-issued:", err)
	}
	// The restored panel's first boot redeploys the controller's tiles.
	if err := os.WriteFile(filepath.Join(dataDir, installspec.RestoredMarker), nil, 0o600); err != nil {
		return err
	}
	if err := r.docker(ctx, installspec.Panel(*image, in).RunArgs()...); err != nil {
		return err
	}
	if err := dropStage(stage); err != nil {
		r.say("  warning: delete", stage, "by hand, it holds unencrypted certificate keys:", err)
	}
	// The CLI key on this box lived in the replaced database (and, on a new
	// box, in a cli.json that is gone): mint a new one. Not fatal.
	if err := r.relogCLI(ctx, in, *image); err != nil {
		r.say("  cli login:", err.Error(), "- log in by hand:", docsCLILogin)
	}
	r.say("Restored", fs.Arg(0), "on", *image+". The replaced database is", replaced)
	return nil
}

// passphraseEnv carries the recovery passphrase without a flag (ps shows
// flags to every user on the box).
const passphraseEnv = "STACKR_RECOVERY_PASSPHRASE"

// passphrase is the recovery passphrase from the flag, the environment or
// the first line of stdin, in that order; "" = use this box's master key.
func passphrase(flagVal, envVal string, fromStdin bool, stdin io.Reader) (string, error) {
	switch {
	case flagVal != "":
		return flagVal, nil
	case envVal != "":
		return envVal, nil
	case fromStdin:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read the passphrase from stdin: %w", err)
		}
		if line = strings.TrimRight(line, "\r\n"); line == "" {
			return "", errors.New("--passphrase-stdin: stdin held no passphrase")
		}
		return line, nil
	}
	return "", nil
}

// relogCLI mints the admin a new API key in the restored panel and writes
// this box's cli.json with it, the way the install does. Only this
// installer's own build is asked: an older stackrd has no admin-key and
// would boot a second server on the same database instead.
func (r runner) relogCLI(ctx context.Context, in installspec.Input, image string) error {
	own, err := checkVersion("", version)
	if err != nil || installspec.Image(own) != image {
		return fmt.Errorf("the panel runs %s, not this installer's build", image)
	}
	if err := waitHealthy(ctx, "http://"+net.JoinHostPort(in.Bind, installspec.PanelPort)+"/api/health"); err != nil {
		return err
	}
	panel, err := read(ctx, "docker", "ps", "-q", "--filter", "label="+installspec.LabelRole+"=panel")
	if err != nil || panel == "" {
		return errors.New("the panel container is not running")
	}
	b, err := exec.CommandContext(ctx, "docker", "exec", strings.Fields(panel)[0], "/stackrd", "admin-key").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	key := adminKey(string(b))
	if key == "" {
		return errors.New("the panel minted no key")
	}
	if err := r.loginCLI(in, key); err != nil {
		return err
	}
	r.say("  the CLI on this box is logged in as the admin again")
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
const caddyMember = installspec.CaddyMember

// unpack decrypts a panel archive into dir and returns the build it names.
// Only the members of installspec.PanelState are accepted; the optional ones
// (cluster keys, the proxy volume's tar) may be missing.
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
	want, optional := map[string]os.FileMode{}, map[string]os.FileMode{}
	for _, m := range installspec.PanelState {
		if m.Optional {
			optional[m.Name] = os.FileMode(m.Mode)
		} else {
			want[m.Name] = os.FileMode(m.Mode)
		}
	}
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
