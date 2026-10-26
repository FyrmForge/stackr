package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/FyrmForge/stackr/internal/installspec"
)

func panelNames() []string {
	var n []string
	for _, m := range installspec.PanelState {
		n = append(n, m.Name)
	}
	return n
}

func newKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func archive(t *testing.T, id *age.X25519Identity, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	enc, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	for _, name := range append(panelNames(), "../evil", "keys/other.key", "keys/recovery.pub") {
		body, ok := files[name]
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	for _, c := range []interface{ Close() error }{tw, gz, enc} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return &buf
}

func TestUnpack(t *testing.T) {
	good := map[string]string{"stackr.db": "db", "keys/master.key": "k\n", "VERSION": "v0.1.0\n", "RECOVERY": "3\n", "MASTERGEN": "2\n", "INSTALLID": "b1195bbc\n"}
	k := newKey(t)
	dir := t.TempDir()
	info, err := unpack(archive(t, k, good), k, dir)
	if err != nil || info.version != "v0.1.0" || info.gen != 3 || info.masterGen != 2 || info.installID != "b1195bbc" {
		t.Fatalf("unpack = %+v %v", info, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keys/master.key")); string(b) != "k\n" {
		t.Errorf("master key = %q", b)
	}

	// Only the recovery key opens it; the failure names the generation the
	// archive's name carries.
	_, err = unpack(archive(t, k, good), newKey(t), t.TempDir())
	if err == nil {
		t.Fatal("a different recovery key opened the archive")
	}
	if got := withNeededGen(err, "p/scheduled/20261017-030000-panel-g3.tar.gz.age").Error(); !strings.Contains(got, "generation 3") {
		t.Errorf("message = %q", got)
	}
	if got := withNeededGen(err, "panel.tar.gz.age").Error(); strings.Contains(got, "generation") {
		t.Errorf("a name with no generation was guessed at: %q", got)
	}
	evil := map[string]string{
		"stackr.db":       "db",
		"keys/master.key": "k",
		"VERSION":         "v0.1.0",
		"RECOVERY":        "1",
		"MASTERGEN":       "1",
		"../evil":         "x",
	}
	if _, err := unpack(archive(t, k, evil), k, t.TempDir()); err == nil {
		t.Error("entry outside the panel members accepted")
	}
	if _, err := unpack(archive(t, k, map[string]string{"VERSION": "v0.1.0"}), k, t.TempDir()); err == nil {
		t.Error("archive without a database accepted")
	}
}

// recovery.pub is no archive member: an archive carrying one is refused and
// a normal one stages none (restore writes it from the key it was opened with).
func TestRecoveryPubIsNoArchiveMember(t *testing.T) {
	k := newKey(t)
	files := map[string]string{"stackr.db": "db", "keys/master.key": "k\n", "VERSION": "v0.1.0\n", "RECOVERY": "1\n", "MASTERGEN": "1\n"}
	stage := t.TempDir()
	if _, err := unpack(archive(t, k, files), k, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, installspec.RecoveryPubFile)); err == nil {
		t.Error("the stage holds a recovery.pub")
	}
	for _, m := range installspec.PanelState {
		if m.Name == installspec.RecoveryPubFile || m.Name == installspec.RecoveryPendingFile || m.Name == installspec.KeyNextFile {
			t.Errorf("%s is part of the panel archive", m.Name)
		}
	}
	files["keys/recovery.pub"] = "age1evil\n1\n"
	if _, err := unpack(archive(t, k, files), k, t.TempDir()); err == nil {
		t.Error("an archive carrying keys/recovery.pub was accepted")
	}
}

// An archive with the proxy's volume stages it; one without is as before.
// restoreCaddy stops the proxy, untars into its volume, starts it.
func TestRestoreCaddy(t *testing.T) {
	files := map[string]string{"stackr.db": "db", "keys/master.key": "k\n", "VERSION": "v0.1.0\n", "RECOVERY": "1\n", "MASTERGEN": "1\n", "caddy.tar.gz": "tarball"}
	dir, k := t.TempDir(), newKey(t)
	if _, err := unpack(archive(t, k, files), k, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "caddy.tar.gz")); string(b) != "tarball" {
		t.Errorf("staged caddy = %q", b)
	}
	var out bytes.Buffer
	if err := restoreCaddy(context.Background(), runner{dry: true, out: &out}, "img:1", dir); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	stop, untar, start := strings.Index(got, "stop stackr-proxy"), strings.Index(got, "stackr-caddy:/data"), strings.Index(got, "start stackr-proxy")
	if stop < 0 || untar < stop || start < untar {
		t.Errorf("order wrong:\n%s", got)
	}
	empty := t.TempDir()
	out.Reset()
	if err := restoreCaddy(context.Background(), runner{dry: true, out: &out}, "img:1", empty); err != nil || out.Len() != 0 {
		t.Errorf("no caddy member: %v %q", err, out.String())
	}
}

// untarScript wipes and untars; when the untar fails the tree that was there
// comes back, so a bad tarball never costs the certificates.
func TestUntarScriptKeepsTreeOnFailure(t *testing.T) {
	run := func(dir string, stdin []byte) error {
		cmd := exec.Command("sh", "-c", untarScript(dir))
		cmd.Stdin = bytes.NewReader(stdin)
		return cmd.Run()
	}
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "caddy", "certificates"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "caddy", "certificates", "a.crt"), []byte("pem"), 0o600))
	must(os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o600))

	if err := run(dir, []byte("not a tarball")); err == nil {
		t.Error("a bad tarball reported success")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "caddy", "certificates", "a.crt")); string(b) != "pem" {
		t.Errorf("the old tree was not put back: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".hidden")); string(b) != "h" {
		t.Errorf("the old dotfile was not put back: %q", b)
	}

	// A good tarball replaces the tree.
	src := t.TempDir()
	must(os.WriteFile(filepath.Join(src, "new.crt"), []byte("new"), 0o600))
	var tarball bytes.Buffer
	cmd := exec.Command("tar", "-czf", "-", "-C", src, ".")
	cmd.Stdout = &tarball
	must(cmd.Run())
	must(run(dir, tarball.Bytes()))
	if _, err := os.Stat(filepath.Join(dir, "caddy")); err == nil {
		t.Error("the old tree survived a good restore")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new.crt")); string(b) != "new" {
		t.Errorf("new tree = %q", b)
	}
}

// The staging dir holds unencrypted certificate keys; a restore that went
// through deletes it.
func TestDropStage(t *testing.T) {
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "caddy.tar.gz"), []byte("keys"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dropStage(stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); err == nil {
		t.Error("the staging dir is still there")
	}
}

// Every member of installspec.PanelState unpacks (the cluster keys among
// them), optional ones may be missing, and a keys/ file outside the list is
// refused. The backup side's test builds its archive from the same list.
func TestUnpackPanelState(t *testing.T) {
	all := map[string]string{}
	for _, m := range installspec.PanelState {
		all[m.Name] = "body-" + m.Name
	}
	all["VERSION"] = "v0.1.0\n"
	all["RECOVERY"] = "1\n"
	dir, k := t.TempDir(), newKey(t)
	if _, err := unpack(archive(t, k, all), k, dir); err != nil {
		t.Fatal(err)
	}
	for _, m := range installspec.PanelState {
		if m.Name == "VERSION" || m.Name == "RECOVERY" || m.Name == "MASTERGEN" || m.Name == "INSTALLID" {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, m.Name)); err != nil || string(b) != "body-"+m.Name {
			t.Errorf("%s = %q %v", m.Name, b, err)
		}
	}
	req := map[string]string{}
	for _, m := range installspec.PanelState {
		if !m.Optional {
			req[m.Name] = "x"
		}
	}
	req["VERSION"] = "v0.1.0\n"
	req["RECOVERY"], req["MASTERGEN"] = "1\n", "1\n"
	if _, err := unpack(archive(t, k, req), k, t.TempDir()); err != nil {
		t.Errorf("required members alone refused: %v", err)
	}
	req["keys/other.key"] = "x"
	if _, err := unpack(archive(t, k, req), k, t.TempDir()); err == nil {
		t.Error("a keys/ file outside installspec.PanelState was accepted")
	}
}

// The environment wins, then stdin when asked; nothing given is "" (the
// caller prompts). Stdin is never read unasked.
func TestRecoveryKey(t *testing.T) {
	for _, c := range []struct {
		env      string
		stdin    bool
		in, want string
		err      bool
	}{
		{env: " e ", stdin: true, in: "s\n", want: "e"},
		{stdin: true, in: "s p\r\nmore\n", want: "s p"},
		{stdin: true, in: "last", want: "last"},
		{stdin: true, in: "\n", err: true},
		{in: "s\n", want: ""},
	} {
		got, err := recoveryKey(c.env, c.stdin, strings.NewReader(c.in))
		if (err != nil) != c.err || got != c.want {
			t.Errorf("recoveryKey(%+v) = %q, %v; want %q", c, got, err, c.want)
		}
	}
}

// A prefix at the panel archives' root picks the only install there, or
// names the installs to choose from; any other prefix is left alone.
func TestPickInstall(t *testing.T) {
	root := "stackr/_panel/"
	one := []string{root + "aa/scheduled/20261017-030000-panel-g1.tar.gz.age", root + "aa/manual/20261018-030000-panel-g1.tar.gz.age"}
	got, err := pickInstall(root, one)
	if err != nil || newestKey(got) != one[1] {
		t.Errorf("one install: %v %v", got, err)
	}
	two := append(slices.Clone(one), root+"bb/scheduled/20261019-030000-panel-g1.tar.gz.age")
	if _, err := pickInstall(root, two); err == nil || !strings.Contains(err.Error(), "aa, bb") {
		t.Errorf("two installs: %v", err)
	}
	if got, err := pickInstall(root+"aa/", two); err != nil || len(got) != len(two) {
		t.Errorf("an install prefix was narrowed: %v %v", got, err)
	}
}

func TestParseS3(t *testing.T) {
	for in, want := range map[string][2]string{
		"s3://b/stackr/_panel/x/scheduled/a.age": {"b", "stackr/_panel/x/scheduled/a.age"},
		"s3://b/stackr/_panel/x/":                {"b", "stackr/_panel/x/"},
		"s3://b":                                 {"b", ""},
	} {
		b, k, err := parseS3(in)
		if err != nil || b != want[0] || k != want[1] {
			t.Errorf("parseS3(%q) = %q, %q, %v", in, b, k, err)
		}
	}
	for _, bad := range []string{"https://b/k", "s3:///k"} {
		if _, _, err := parseS3(bad); err == nil {
			t.Errorf("parseS3(%q) took it", bad)
		}
	}
}

// The newest archive is the greatest timestamp, whatever trigger dir holds it.
func TestNewestKey(t *testing.T) {
	keys := []string{
		"p/scheduled/20261017-030000-panel.tar.gz.age",
		"p/pre-upgrade/20261017-120000-panel.tar.gz.age",
		"p/manual/20261016-235959-panel.tar.gz.age",
		"p/manual/",
	}
	if got := newestKey(keys); got != keys[1] {
		t.Errorf("newestKey = %q, want %q", got, keys[1])
	}
	if got := newestKey(nil); got != "" {
		t.Errorf("newestKey(nil) = %q", got)
	}
}

// No keys in the environment: refused before any call.
func TestFetchS3NeedsKeys(t *testing.T) {
	env := map[string]string{s3EndpointEnv: "http://127.0.0.1:1", s3AccessKeyEnv: "a"}
	_, _, err := fetchS3(context.Background(), "s3://b/k", t.TempDir(), func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), s3SecretKeyEnv) {
		t.Fatalf("err = %v, want one naming %s", err, s3SecretKeyEnv)
	}
}

// The key the archive was opened with becomes the box's recovery key and a
// fresh install's unsaved key goes; a box on a newer generation keeps its own.
func TestAdoptRecovery(t *testing.T) {
	data, k := t.TempDir(), newKey(t)
	fresh := newKey(t)
	if err := installspec.SaveRecovery(data, fresh.Recipient().String(), 1); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(data, installspec.RecoveryPendingFile)
	if err := os.WriteFile(pending, []byte(fresh.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, err := adoptRecovery(data, k, 2)
	if err != nil || !strings.Contains(msg, "restored with (generation 2)") {
		t.Fatalf("adopt = %q %v", msg, err)
	}
	if pub, gen, _ := installspec.LoadRecovery(data); pub != k.Recipient().String() || gen != 2 {
		t.Errorf("recovery.pub = %s gen %d", pub, gen)
	}
	if _, err := os.Stat(pending); err == nil {
		t.Error("the fresh install's pending key was kept")
	}
	if err := installspec.SaveRecovery(data, fresh.Recipient().String(), 3); err != nil {
		t.Fatal(err)
	}
	if msg, _ := adoptRecovery(data, k, 2); !strings.Contains(msg, "WARNING") {
		t.Errorf("rollback over a rotation = %q", msg)
	}
	if pub, gen, _ := installspec.LoadRecovery(data); pub != fresh.Recipient().String() || gen != 3 {
		t.Errorf("a newer box key was replaced: %s gen %d", pub, gen)
	}
}

// The install id comes from the archive's member, else its name; junk is none.
func TestRestoredInstallID(t *testing.T) {
	for _, tt := range []struct {
		member, name, want string
	}{
		{"b1195bbc", "/tmp/x.age", "b1195bbc"},
		{"", "stackr/_panel/b1195bbc/manual/20261026-165728-panel-g2.tar.gz.age", "b1195bbc"},
		{"", "/var/lib/stackr/backups/stackr/_panel/default/upgrade/x.age", "default"},
		{"", "/root/panel.tar.gz.age", ""},
		{"a b", "", ""},
	} {
		if got := restoredInstallID(archiveInfo{installID: tt.member}, tt.name); got != tt.want {
			t.Errorf("%q %q = %q, want %q", tt.member, tt.name, got, tt.want)
		}
	}
	if got := markerBody(42, "/x/backups/stackr/_panel/i1/manual/a.age"); got != "42\nstackr/_panel/i1/manual/a.age\n" {
		t.Errorf("marker = %q", got)
	}
}
