package operated

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

func helperBuild(m string, extra map[string]string, recipes ...string) ExtBuild {
	p := map[string]string{"postgresql-" + m: "1.2-1", "postgresql-client-" + m: "1.2-1", "postgresql-common": "300", "postgresql-client-common": "300"}
	maps.Copy(p, extra)
	return ExtBuild{Base: "postgres:" + m, Ref: "r", Recipes: recipes, Packages: p}
}

func TestHelperDockerfile(t *testing.T) {
	from := helperBuild("17", map[string]string{"postgresql-17-pgvector": "0.8.7-1.pgdg13+1", "postgresql-17-vchord": "1.1.1-1"}, "pgvector", "vchord")
	to := helperBuild("18", map[string]string{"postgresql-18-pgvector": "0.8.7-1.pgdg13+1", "postgresql-18-vchord": "1.1.1-1"}, "pgvector", "vchord")
	df, err := HelperDockerfile(from, to, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM " + PGUpgradeImage + "\n",
		"--allow-downgrades postgresql-17=1.2-1 postgresql-client-17=1.2-1 postgresql-18=1.2-1 postgresql-client-18=1.2-1 postgresql-client-common=300 postgresql-common=300",
		"apt-mark hold postgresql-common postgresql-client-common postgresql-17 postgresql-client-17 postgresql-18 postgresql-client-18",
		"postgresql-17-pgvector=0.8.7-1.pgdg13+1 postgresql-18-pgvector=0.8.7-1.pgdg13+1",
		"ADD --checksum=sha256:a48132c8d616c0d52a9811b32f12f88a8f6cc8542d7cc0180c8d0986bfbd03c5 https://github.com/supervc-stack/VectorChord/releases/download/1.1.1/postgresql-17-vchord_1.1.1-1_amd64.deb /tmp/ext-17-0.deb\n",
		"ADD --checksum=sha256:7637a18a97157db8d904bb7093e5ce33b6a395c0acac9ae6b660032f3d57a33e https://github.com/supervc-stack/VectorChord/releases/download/1.1.1/postgresql-18-vchord_1.1.1-1_amd64.deb /tmp/ext-18-1.deb\n",
		"apt-get install -y /tmp/ext-17-0.deb /tmp/ext-18-1.deb",
		"postgresql-17-vchord=1.1.1-1 postgresql-18",
		"version differs from its pin",
		"/usr/local/bin/gosu",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, df)
		}
	}
	if strings.Contains(strings.SplitN(df, "ADD", 2)[0], "vchord=") {
		t.Error("a URL package is apt-installed by name")
	}
	if again, _ := HelperDockerfile(from, to, "amd64"); again != df {
		t.Error("not deterministic")
	}
	if _, err := HelperDockerfile(from, to, "riscv64"); err == nil {
		t.Error("an arch with no checksum was accepted")
	}
	// the two images may carry different builds of the shared common
	// packages: the new major's wins, nothing is refused
	from.Packages["postgresql-common"] = "290"
	if df, err := HelperDockerfile(from, to, "amd64"); err != nil || strings.Contains(df, "=290") {
		t.Errorf("older common in the old build: %v\n%s", err, df)
	}
}

func TestHelperDockerfileRefusals(t *testing.T) {
	ok17, ok18 := helperBuild("17", nil), helperBuild("18", nil)
	if _, err := HelperDockerfile(ok17, ok18, "amd64"); err != nil {
		t.Fatalf("plain: %v", err)
	}
	if _, err := HelperDockerfile(ExtBuild{}, ok18, "amd64"); err == nil {
		t.Error("no server accepted")
	}
	if _, err := HelperDockerfile(helperBuild("17", nil, "nope"), ok18, "amd64"); err == nil {
		t.Error("unknown recipe accepted")
	}
	if _, err := HelperDockerfile(helperBuild("17", nil, "vchord"), ok18, "amd64"); err == nil {
		t.Error("vchord build without its recorded version accepted")
	}
	for _, bad := range hostileMore {
		if _, err := HelperDockerfile(helperBuild("17", map[string]string{"postgresql-17-pgvector": bad}), ok18, "amd64"); err == nil {
			t.Errorf("version %q accepted", bad)
		}
		if _, err := HelperDockerfile(helperBuild("17", map[string]string{bad: "1"}), ok18, "amd64"); err == nil {
			t.Errorf("package %q accepted", bad)
		}
		if _, err := HelperDockerfile(ok17, ok18, bad); err == nil {
			t.Errorf("arch %q accepted", bad)
		}
	}
	// a custom URL recipe: a hostile checksum never reaches the text
	r := Recipe{Name: "mine", Majors: []int{17}, PackageURL: "https://x.test/{version}/postgresql-{major}-mine_{version}_{arch}.deb", Version: "1", Extensions: []string{"mine"},
		SHA256: map[string]string{"17-amd64": strings.Repeat("a", 64), "17-arm64": strings.Repeat("b", 64)}}
	b := helperBuild("17", map[string]string{"postgresql-17-mine": "1"}, "mine")
	if df, err := HelperDockerfile(b, ok18, "amd64", r); err != nil || !strings.Contains(df, "ADD --checksum=sha256:"+strings.Repeat("a", 64)+" https://x.test/1/postgresql-17-mine_1_amd64.deb") {
		t.Errorf("custom url recipe: %v\n%s", err, df)
	}
	r.SHA256["17-amd64"] = "a\nRUN evil"
	if _, err := HelperDockerfile(b, ok18, "amd64", r); err == nil {
		t.Error("hostile checksum accepted")
	}
}

// The helper fetches the file the build recorded, not what the recipe says
// now; a record without URLs falls back to the recipe.
func TestHelperDockerfileUsesRecordedURL(t *testing.T) {
	from := helperBuild("17", map[string]string{"postgresql-17-pgvector": "0.8.7-1", "postgresql-17-vchord": "1.0.9-1"}, "pgvector", "vchord")
	to := helperBuild("18", map[string]string{"postgresql-18-pgvector": "0.8.7-1", "postgresql-18-vchord": "1.0.9-1"}, "pgvector", "vchord")
	old := BuiltinRecipes[2]
	old.Version = "1.0.9"
	var err error
	if from.URLs, err = URLPackages([]Recipe{old}, 17, "amd64"); err != nil {
		t.Fatal(err)
	}
	rec := from.URLs["postgresql-17-vchord"]
	rec.SHA256 = strings.Repeat("ab", 32)
	from.URLs["postgresql-17-vchord"] = rec
	df, err := HelperDockerfile(from, to, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df, "sha256:"+rec.SHA256+" "+rec.URL+" ") || !strings.Contains(rec.URL, "/1.0.9/") {
		t.Errorf("recorded file not used:\n%s", df)
	}
	if !strings.Contains(df, "/1.1.1/postgresql-18-vchord_1.1.1-1_amd64.deb") {
		t.Errorf("the new major (no record) did not use the recipe:\n%s", df)
	}
}

// A build record with its own release files needs no recipe: one removed or
// edited to drop the old major since still gives the helper (Codex review 2).
func TestHelperDockerfileRecordWithoutRecipe(t *testing.T) {
	from := helperBuild("17", map[string]string{"postgresql-17-mine": "2.0-1"}, "mine")
	to := helperBuild("18", map[string]string{"postgresql-18-mine": "2.0-1"}, "mine")
	for i, b := range []*ExtBuild{&from, &to} {
		m := 17 + i
		b.URLs = map[string]URLPkg{fmt.Sprintf("postgresql-%d-mine", m): {
			URL: fmt.Sprintf("https://example.com/mine/postgresql-%d-mine_2.0-1_amd64.deb", m), SHA256: strings.Repeat("cd", 32), Version: "2.0"}}
	}
	df, err := HelperDockerfile(from, to, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df, "postgresql-17-mine_2.0-1_amd64.deb") || !strings.Contains(df, "postgresql-18-mine_2.0-1_amd64.deb") {
		t.Errorf("recorded files not used:\n%s", df)
	}
}
