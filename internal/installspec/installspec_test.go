package installspec

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

var tlsIn = Input{
	Root:         "example.com",
	PanelHost:    "stkr.example.com",
	HTTPS:        true,
	Email:        "ops@example.com",
	Proxies:      "203.0.113.0/24",
	HTTPPort:     "80",
	HTTPSPort:    "443",
	DataDir:      "/var/lib/stackr",
	Bind:         "172.17.0.1",
	BridgeSubnet: "172.17.0.0/16",
}

var plainIn = Input{
	Root:      "*.example.com", // the panel env carries it bare
	PanelHost: "stkr.example.com",
	HTTPPort:  "8081",
	DataDir:   "/srv/stackr",
	Bind:      "172.18.0.1",
}

// The docker run lines are the contract with the box; a changed flag shows
// up as a golden diff.
func TestGolden(t *testing.T) {
	for name, c := range map[string]Container{
		"panel_tls":   Panel(Image("v1.2.3"), tlsIn),
		"panel_plain": Panel(Image("1.2.3"), plainIn),
		"proxy_tls":   Proxy(Image("1.2.3"), tlsIn),
		"proxy_plain": Proxy(Image("1.2.3"), plainIn),
	} {
		t.Run(name, func(t *testing.T) {
			got := "docker " + strings.Join(c.RunArgs(), "\n  ") + "\n"
			file := filepath.Join("testdata", name+".txt")
			if *update {
				if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != got {
				t.Errorf("%s differs:\n%s", file, got)
			}
		})
	}
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[Input]string{
		{PanelHost: "p.x.com", HTTPS: true, HTTPSPort: "443"}:  "https://p.x.com",
		{PanelHost: "p.x.com", HTTPS: true, HTTPSPort: "8443"}: "https://p.x.com:8443",
		{PanelHost: "p.x.com", HTTPPort: "80"}:                 "http://p.x.com",
		{PanelHost: "p.x.com", HTTPPort: "8080"}:               "http://p.x.com:8080",
	} {
		if got := in.BaseURL(); got != want {
			t.Errorf("%+v: %s, want %s", in, got, want)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	in := tlsIn
	in.DataDir = t.TempDir()
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := Load(in.DataDir)
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestCheckRoot(t *testing.T) {
	for _, bad := range []string{
		"",
		"localhost",
		"127.0.0.1",
		"::1",
		"example",
		"ex ample.com",
		"-bad.com",
		"a..b.com",
		"example.123",
		strings.Repeat("a", 64) + ".com",
	} {
		if got, err := CheckRoot(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	for in, want := range map[string]string{
		"https://Example.COM/path": "example.com",
		"HTTPS://Example.COM/":     "example.com",
		"example.com.":             "example.com",
		"example.com:8443":         "example.com",
		"*.apps.example.com":       "*.apps.example.com",
	} {
		if got, err := CheckRoot(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
}

// The docker CLI config sits on the data dir, so buildx's builder registry
// survives a panel upgrade.
func TestPanelDockerConfigOnDataDir(t *testing.T) {
	if want := "DOCKER_CONFIG=" + plainIn.DataDir + "/docker"; !slices.Contains(Panel(Image("1.2.3"), plainIn).Env, want) {
		t.Errorf("panel env lacks %s: %v", want, Panel(Image("1.2.3"), plainIn).Env)
	}
}

// The panel's env carries the install id, so archives are named by it; an
// install without one sets nothing and the panel keeps "default".
func TestPanelInstallID(t *testing.T) {
	has := func(in Input) bool {
		return slices.Contains(Panel(Image("1.2.3"), in).Env, "STACKR_INSTALL_ID="+in.InstallID)
	}
	withID := plainIn
	withID.InstallID = "9f2c41d7a0b34e5a"
	if !has(withID) {
		t.Errorf("panel env lacks the install id: %v", Panel(Image("1.2.3"), withID).Env)
	}
	for _, e := range Panel(Image("1.2.3"), plainIn).Env {
		if strings.HasPrefix(e, "STACKR_INSTALL_ID") {
			t.Errorf("an install without an id sets %q", e)
		}
	}
}

// The stamp changes with anything the proxy is made from and with nothing
// else, so a re-run or upgrade recreates exactly the stale one.
func TestProxyHashStamp(t *testing.T) {
	in := Input{Root: "x.io", PanelHost: "x.io", HTTPPort: "80", DataDir: "/data"}
	base := Proxy("img:1", in)
	if base.Labels[LabelSpec] == "" || base.Labels[LabelSpec] != Proxy("img:1", in).Labels[LabelSpec] {
		t.Fatal("stamp missing or unstable")
	}
	other := in
	other.DataDir = "/other"
	for name, c := range map[string]Container{
		"image": Proxy("img:2", in),
		"mount": Proxy("img:1", other),
	} {
		if c.Labels[LabelSpec] == base.Labels[LabelSpec] {
			t.Errorf("%s change kept the stamp", name)
		}
	}
}

// Every container the installer makes caps its own logs: the daemon default
// is unbounded.
func TestLogCap(t *testing.T) {
	for name, c := range map[string]Container{
		"panel":     Panel(Image("1.2.3"), tlsIn),
		"proxy":     Proxy(Image("1.2.3"), tlsIn),
		"nodeproxy": NodeProxy(Image("1.2.3")),
		"registry":  Registry("/var/lib/stackr"),
	} {
		got := strings.Join(c.RunArgs(), " ")
		if !strings.Contains(got, "--log-opt max-size=10m --log-opt max-file=3") {
			t.Errorf("%s: run args %q lack the log cap", name, got)
		}
		if c.Labels[LabelSpec] != "" && c.Labels[LabelSpec] != c.Hash() {
			t.Errorf("%s: spec stamp is stale", name)
		}
	}
}

// An empty dir on Docker's disk is mounted read-only at its data root when known and
// not already under the data dir; empty adds nothing.
func TestPanelDockerRoot(t *testing.T) {
	in := plainIn
	base := len(Panel(Image("1.2.3"), in).Volumes)
	in.DockerRoot = "/mnt/docker"
	if v := Panel(Image("1.2.3"), in).Volumes; !slices.Contains(v, "/mnt/docker/.stackr-disk:/mnt/docker:ro") || len(v) != base+1 {
		t.Errorf("volumes %v lack the docker root mount", v)
	}
	for _, r := range []string{"", in.DataDir, in.DataDir + "/docker", "/a:b"} {
		in.DockerRoot = r
		if v := Panel(Image("1.2.3"), in).Volumes; len(v) != base {
			t.Errorf("root %q added a volume: %v", r, v)
		}
	}
}

func TestWriteDurableContentAndMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if err := WriteKeyFile(p, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	in := tlsIn
	in.DataDir = dir
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p, filepath.Join(dir, File)} {
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, st, err)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "secret" {
		t.Errorf("content = %q", b)
	}
	if m, _ := filepath.Glob(p + ".tmp*"); len(m) > 0 {
		t.Errorf("temp file left behind: %v", m)
	}
}

func TestNodeName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Box_01.local": "box-01-local", "-x-": "x", "": "controller", "___": "controller",
		strings.Repeat("a", 31) + "-b": strings.Repeat("a", 31),
	} {
		if got := CleanNodeName(in); got != want {
			t.Errorf("CleanNodeName(%q) = %q, want %q", in, got, want)
		}
		if _, err := CheckNodeName(CleanNodeName(in)); err != nil {
			t.Errorf("cleaned %q fails the check: %v", in, err)
		}
	}
	if _, err := CheckNodeName("Bad_Name"); err == nil {
		t.Error("Bad_Name accepted")
	}
	// An install from before the name existed adds no env, so its container
	// spec (and hash) is unchanged: the controller keeps its name.
	if slices.ContainsFunc(Panel("img", tlsIn).Env, func(e string) bool { return strings.HasPrefix(e, "STACKR_NODE_NAME=") }) {
		t.Error("an input without a node name sets STACKR_NODE_NAME")
	}
	in := tlsIn
	in.NodeName = "box1"
	if !slices.Contains(Panel("img", in).Env, "STACKR_NODE_NAME=box1") {
		t.Error("node name not passed to the panel")
	}
}

// The disks bind is off by default (the golden files) and on with DisksBind,
// as a read-only rslave view under HostRoot.
func TestPanelDisksBind(t *testing.T) {
	in := plainIn
	base := len(Panel(Image("1.2.3"), in).Volumes)
	if v := Panel(Image("1.2.3"), in).Volumes; slices.Contains(v, DisksDir+":"+HostRoot+DisksDir+":ro,rslave") {
		t.Errorf("bind on by default: %v", v)
	}
	in.DisksBind = true
	if v := Panel(Image("1.2.3"), in).Volumes; !slices.Contains(v, DisksDir+":"+HostRoot+DisksDir+":ro,rslave") || len(v) != base+1 {
		t.Errorf("volumes %v lack the disks bind", v)
	}
	in.DataDir = t.TempDir()
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(in.DataDir); err != nil || got.DisksBind {
		t.Errorf("DisksBind was saved: %+v %v", got, err)
	}
}

func TestDisksBindOK(t *testing.T) {
	line := func(id, point, opt string) string {
		return id + " 1 8:1 / " + point + " rw,relatime " + opt + " - ext4 /dev/sda1 rw\n"
	}
	for name, c := range map[string]struct {
		table string
		want  bool
	}{
		"shared root":       {line("1", "/", "shared:1"), true},
		"private root":      {line("1", "/", ""), false},
		"slave root":        {line("1", "/", "master:2"), true},
		"private var/lib":   {line("1", "/", "shared:1") + line("2", "/var/lib", ""), false},
		"shared var":        {line("1", "/", "") + line("2", "/var", "shared:3"), true},
		"unrelated private": {line("1", "/", "shared:1") + line("2", "/var/lib/stackr-disksx", ""), true},
		"disks mount":       {line("1", "/", "") + line("2", "/var/lib/stackr-disks", "shared:4"), true},
		"escaped space":     {line("1", "/", "") + line("2", `/var/my\040dir`, ""), false},
		"garbage":           {"not a mount table\n\x00\xff", false},
		"empty":             {"", false},
	} {
		if got := DisksBindOK([]byte(c.table)); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestWithoutDisksBind(t *testing.T) {
	in := plainIn
	in.DisksBind = true
	v := Panel(Image("1.2.3"), in).Volumes
	got := WithoutDisksBind(v)
	if slices.Contains(got, DisksBindVolume) || len(got) != len(v)-1 || !slices.Contains(v, DisksBindVolume) {
		t.Errorf("got %v from %v", got, v)
	}
}
