package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
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
// the recovery key, and that key stays the box's recovery key (adoptRecovery):
// the one the admin restored with keeps opening new backups. The install id
// comes back too, so backups land in (and prune) the old prefix.
func restore(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stackr-install restore", flag.ContinueOnError)
	keyStdin := fs.Bool("key-stdin", false, "read the recovery key from the first line of stdin (or set "+recoveryKeyEnv+", or type it at the prompt)")
	image := fs.String("image", "", "panel image to run (default: the build the archive names)")
	rotate := fs.Bool("rotate-key", true, "when the archive carries an older master key than this box, rotate to a new one on the first boot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: stackr-install restore [--key-stdin] [--rotate-key=false] [--image REF] <archive | s3://bucket/prefix/>")
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
	if fi, err := f.Stat(); err == nil {
		info.size = fi.Size()
	}
	keyWarn := masterKeyChange(dataDir, stage, info.masterGen)
	if keyWarn != "" {
		_, _ = fmt.Fprintln(out, "Warning:", keyWarn)
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
	// A master key waiting to replace the old one belongs to the box that was
	// replaced; the restored database is sealed with the archive's key.
	if err := os.Remove(filepath.Join(dataDir, installspec.KeyNextFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if keyWarn != "" {
		// The box's generation note is the replaced key's; the archive's
		// stamp is the restored key's (0 = an archive from before the stamp).
		if err := writeMasterGen(dataDir, info.masterGen); err != nil {
			return err
		}
	}
	if keyWarn != "" && *rotate {
		// The first boot re-seals everything under a fresh key (rotation).
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		if err := installspec.WriteKeyFile(filepath.Join(dataDir, installspec.KeyNextFile), []byte(hex.EncodeToString(raw)+"\n")); err != nil {
			return err
		}
		r.say("  the panel re-seals every secret under a new master key on its first boot")
	}
	// Certificates are re-issuable (rate limits aside), the database is not:
	// a proxy volume that fails to restore warns and the restore goes on.
	if err := restoreCaddy(ctx, r, *image, stage); err != nil {
		r.say("  warning: proxy volume not restored, certificates will be re-issued:", err)
	}
	msg, err := adoptRecovery(dataDir, id, info.gen)
	if err != nil {
		return err
	}
	r.say(msg)
	if iid := restoredInstallID(info, archiveName); iid != "" && iid != in.InstallID {
		in.InstallID = iid
		if err := installspec.Save(in); err != nil {
			return err
		}
		r.say("  backups go on under install id", iid, "(the old install's prefix)")
	}
	// The restored panel's first boot redeploys the controller's tiles and
	// closes the panel backup run the archive came from (settleRestored).
	if err := os.WriteFile(filepath.Join(dataDir, installspec.RestoredMarker), []byte(markerBody(info.size, archiveName)), 0o600); err != nil {
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
	org := outLine(string(b), "org: ")
	if err := r.loginCLI(in, key, org); err != nil {
		return err
	}
	if org != "" {
		r.say("  the CLI on this box is logged in as the admin again, on org", org)
	} else {
		r.say("  the CLI on this box is logged in as the admin again")
	}
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
	version   string
	gen       int
	masterGen int    // 0 = the archive predates the stamp
	installID string // "" = the archive predates the stamp
	size      int64  // bytes of the encrypted archive, set by restore
}

// unpack decrypts a panel archive into dir. Only the members of
// installspec.PanelState are accepted; the optional ones (cluster keys, the
// proxy volume's tar) may be missing. VERSION and RECOVERY are read, not
// written (INSTALLID too). recovery.pub is no member: restore writes it from
// the key that opened the archive.
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
		switch h.Name {
		case installspec.VersionMember, installspec.RecoveryMember, installspec.MasterGenMember, installspec.InstallIDMember:
			b, err := io.ReadAll(io.LimitReader(tr, 256))
			if err != nil {
				return info, err
			}
			body := strings.TrimSpace(string(b))
			switch h.Name {
			case installspec.VersionMember:
				info.version = body
			case installspec.RecoveryMember:
				info.gen, _ = strconv.Atoi(body)
			case installspec.InstallIDMember:
				info.installID = body
			default:
				info.masterGen, _ = strconv.Atoi(body)
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

// masterKeyChange says what restoring does to the master key: "" when the
// archive's key (staged in stage) is the one this box runs on, else a warning
// naming the key that replaced it when the box has rotated. thisGen is the
// generation the archive is stamped with, 0 when it has none.
func masterKeyChange(dataDir, stage string, thisGen int) string {
	had, err := os.ReadFile(filepath.Join(dataDir, installspec.KeyFile))
	if err != nil {
		return ""
	}
	got, err := os.ReadFile(filepath.Join(stage, installspec.KeyFile))
	if err != nil || strings.TrimSpace(string(had)) == strings.TrimSpace(string(got)) {
		return ""
	}
	// keys/master.gen is the panel's note of the current generation and date.
	if g, _ := os.ReadFile(filepath.Join(dataDir, "keys/master.gen")); len(strings.Fields(string(g))) == 2 {
		f := strings.Fields(string(g))
		if at, err := time.Parse(time.RFC3339, f[1]); err == nil {
			if thisGen > 0 {
				return fmt.Sprintf("This backup uses master key %d; key %s replaced it on %s; restoring brings key %d back.", thisGen, f[0], at.Format("2006-01-02"), thisGen)
			}
			return fmt.Sprintf("This backup uses an older master key; key %s replaced it on %s; restoring brings the old key back.", f[0], at.Format("2006-01-02"))
		}
	}
	// No rotation on this box: most likely a fresh install, whose own key
	// sealed nothing worth keeping.
	return "The backup brings its own master key; it replaces the key this box was installed with."
}

// writeMasterGen sets keys/master.gen to the restored key's generation, so
// the panel's count (and a rotation's +1) follows the archive's lineage, not
// the replaced box's. gen 0 (an unstamped archive) leaves the note alone;
// gen 1 is "never rotated", which is no note at all.
// ponytail: the date is the restore's; the archive carries no rotation date.
func writeMasterGen(dataDir string, gen int) error {
	p := filepath.Join(dataDir, "keys/master.gen")
	switch {
	case gen == 0:
		return nil
	case gen == 1:
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return installspec.WriteKeyFile(p, []byte(strconv.Itoa(gen)+"\n"+time.Now().UTC().Format(time.RFC3339)+"\n"))
}

// adoptRecovery makes the key that opened the archive (generation gen) the
// box's recovery key, so it keeps opening every new backup, and drops a
// fresh install's unsaved key. A box already on a newer generation (rolled
// back on itself after a rotation) keeps its own. The line says which.
func adoptRecovery(dataDir string, id *age.X25519Identity, gen int) (string, error) {
	gen = max(gen, 1)
	pub, boxGen, err := installspec.LoadRecovery(dataDir)
	if err != nil {
		return "", err
	}
	restored := id.Recipient().String()
	switch {
	case pub == restored:
		return fmt.Sprintf("  new backups stay encrypted to the recovery key you restored with (generation %d)", gen), nil
	case pub != "" && boxGen > gen:
		return fmt.Sprintf("  WARNING: new backups are encrypted to this box's recovery key (generation %d), not the one you restored with (generation %d)", boxGen, gen), nil
	}
	if err := installspec.SaveRecovery(dataDir, restored, gen); err != nil {
		return "", err
	}
	if err := os.Remove(filepath.Join(dataDir, installspec.RecoveryPendingFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return fmt.Sprintf("  new backups stay encrypted to the recovery key you restored with (generation %d); a key this box's install printed opens nothing", gen), nil
}

// restoredInstallID is the install id the archive came from: its INSTALLID
// member, else the <id> in a stackr/_panel/<id>/ name. "" = unknown.
func restoredInstallID(info archiveInfo, name string) string {
	id := info.installID
	if id == "" {
		if m := installIDInName.FindStringSubmatch(name); m != nil {
			id = m[1]
		}
	}
	if !validInstallID.MatchString(id) {
		return ""
	}
	return id
}

var (
	installIDInName = regexp.MustCompile(`(?:^|/)_panel/([^/]+)/`)
	validInstallID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// markerBody is the restored marker's content: the archive's size and, when
// its name holds one, its object key (stackr/_panel/...), for the backup run
// the archive was taken by, whose row was copied mid-run.
func markerBody(size int64, name string) string {
	key := ""
	if i := strings.Index(name, "stackr/_panel/"); i >= 0 {
		key = name[i:]
	}
	return strconv.FormatInt(size, 10) + "\n" + key + "\n"
}
