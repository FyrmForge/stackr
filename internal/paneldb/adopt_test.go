package paneldb

import (
	"os"
	"testing"
)

// A dev checkout moves its own old-path database into db/; anything that looks
// like install state (a journal, a filled db/) is left alone.
func TestAdoptDev(t *testing.T) {
	dir := t.TempDir()
	write(t, Legacy(dir), "db")
	write(t, Legacy(dir)+"-wal", "wal")
	write(t, Legacy(dir)+"-shm", "shm")
	moved, err := AdoptDev(dir)
	if err != nil || !moved {
		t.Fatalf("moved=%v err=%v", moved, err)
	}
	for suf, want := range map[string]string{"": "db", "-wal": "wal", "-shm": "shm"} {
		if b, _ := os.ReadFile(Path(dir) + suf); string(b) != want {
			t.Errorf("%s = %q", suf, b)
		}
	}
	if fi, err := os.Stat(Legacy(dir)); err != nil || !fi.IsDir() {
		t.Error("no placeholder")
	}
	if err := Check(dir, Path(dir)); err != nil {
		t.Error(err)
	}
	if moved, _ := AdoptDev(dir); moved {
		t.Error("moved twice")
	}

	withJournal := t.TempDir()
	write(t, Legacy(withJournal), "db")
	must(t, ClearJournal(withJournal))
	if moved, _ := AdoptDev(withJournal); moved {
		t.Error("moved with a journal present")
	}
	filled := t.TempDir()
	write(t, Legacy(filled), "db")
	write(t, Path(filled), "other")
	if moved, _ := AdoptDev(filled); moved {
		t.Error("moved over a filled db/")
	}
}
