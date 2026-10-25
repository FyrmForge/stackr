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
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"golang.org/x/term"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// restore puts the install back to a panel archive (a pre-upgrade one, or
// any panel backup): the database and master key from the archive, the panel
// container on the build the archive names. The way back when an upgraded
// panel passed its gate and still turned out broken. The archive opens with
// the recovery key; this box's own recovery.pub is kept, never replaced from
// the archive.
func restore(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stackr-install restore", flag.ContinueOnError)
	keyStdin := fs.Bool("key-stdin", false, "read the recovery key from the first line of stdin (or set "+recoveryKeyEnv+", or type it at the prompt)")
	image := fs.String("image", "", "panel image to run (default: the build the archive names)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: stackr-install restore [--key-stdin] [--image REF] <archive | s3://bucket/prefix/>")
	}
	secret, err := recoveryKey(os.Getenv(recoveryKeyEnv), *keyStdin, os.Stdin)
	if err == nil && secret == "" {
		secret, err = promptRecoveryKey(out)
	}
	if err != nil {
		return err
	}
	id, err := age.ParseX25519Identity(secret)
	if err != nil {
		return errors.New("that is not a recovery key (it starts with AGE-SECRET-KEY-)")
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
	src := fs.Arg(0)
	archiveName := src
	if strings.HasPrefix(src, "s3://") {
		tmp, key, err := fetchS3(ctx, src, dataDir, os.Getenv)
		if err != nil {
			return fmt.Errorf("fetch the archive (nothing changed): %w", err)
		}
		defer func() { _ = os.Remove(tmp) }()
		_, _ = fmt.Fprintln(out, "Fetched", key)
		src, archiveName = tmp, key
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	stage := filepath.Join(dataDir, "restore-"+time.Now().UTC().Format("20060102T150405"))
	info, err := unpack(f, id, stage)
	if err != nil {
		return fmt.Errorf("read archive (nothing changed): %w", withNeededGen(err, archiveName))
	}
	ver := info.version
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
	// A master key waiting to replace the old one belongs to the box that was
	// replaced; the restored database is sealed with the archive's key.
	if err := os.Remove(filepath.Join(dataDir, installspec.KeyNextFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
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

// recoveryKeyEnv carries the recovery key without a flag (ps shows flags
// to every user on the box).
const recoveryKeyEnv = "STACKR_RECOVERY_KEY"

// recoveryKey is the recovery key from the environment or, when asked, the
// first line of stdin; "" = neither, so the caller prompts.
func recoveryKey(envVal string, fromStdin bool, stdin io.Reader) (string, error) {
	switch {
	case envVal != "":
		return strings.TrimSpace(envVal), nil
	case fromStdin:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read the recovery key from stdin: %w", err)
		}
		if line = strings.TrimSpace(line); line == "" {
			return "", errors.New("--key-stdin: stdin held no recovery key")
		}
		return line, nil
	}
	return "", nil
}

// promptRecoveryKey asks at the terminal, hidden; a pipe has no prompt.
func promptRecoveryKey(out io.Writer) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no recovery key: set " + recoveryKeyEnv + ", pass --key-stdin, or run this at a terminal")
	}
	_, _ = fmt.Fprint(out, "Recovery key: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(out)
	return strings.TrimSpace(string(b)), err
}

// archiveGen is the recovery key generation in an archive's name
// (...-panel-g2.tar.gz.age); 0 when the name carries none.
func archiveGen(name string) int {
	m := genInName.FindStringSubmatch(path.Base(name))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

var genInName = regexp.MustCompile(`-panel-g(\d+)\.tar\.gz\.age$`)

// withNeededGen turns "no identity matched" into what to do about it: name
// the key generation the archive needs when its name says.
func withNeededGen(err error, name string) error {
	var nm *age.NoIdentityMatchError
	if !errors.As(err, &nm) {
		return err
	}
	if g := archiveGen(name); g > 0 {
		return fmt.Errorf("this recovery key does not open the archive; it needs the key of generation %d", g)
	}
	return errors.New("this recovery key does not open the archive")
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

// archiveInfo is what an archive says about itself: the build it names and
// the recovery key generation it is stamped with.
type archiveInfo struct {
	version string
	gen     int
}

// unpack decrypts a panel archive into dir. Only the members of
// installspec.PanelState are accepted; the optional ones (cluster keys, the
// proxy volume's tar) may be missing. VERSION and RECOVERY are read, not
// written, and recovery.pub is no member, so the box keeps its own.
func unpack(src io.Reader, id age.Identity, dir string) (archiveInfo, error) {
	var info archiveInfo
	plain, err := age.Decrypt(src, id)
	if err != nil {
		return info, err
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return info, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
		return info, err
	}
	want, optional := map[string]os.FileMode{}, map[string]os.FileMode{}
	for _, m := range installspec.PanelState {
		if m.Optional {
			optional[m.Name] = os.FileMode(m.Mode)
		} else {
			want[m.Name] = os.FileMode(m.Mode)
		}
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return info, err
		}
		mode, ok := want[h.Name]
		if !ok {
			mode, ok = optional[h.Name]
		}
		if !ok || h.Typeflag != tar.TypeReg {
			return info, fmt.Errorf("unexpected entry %q; not a panel archive", h.Name)
		}
		delete(want, h.Name)
		if h.Name == installspec.VersionMember || h.Name == installspec.RecoveryMember {
			b, err := io.ReadAll(io.LimitReader(tr, 256))
			if err != nil {
				return info, err
			}
			if h.Name == installspec.VersionMember {
				info.version = strings.TrimSpace(string(b))
			} else {
				info.gen, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			}
			continue
		}
		w, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return info, err
		}
		_, err = io.Copy(w, tr)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return info, err
		}
	}
	if len(want) > 0 {
		return info, errors.New("the archive is incomplete")
	}
	return info, nil
}
