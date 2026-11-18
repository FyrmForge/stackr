package render_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// A disk job reads disk.<verb>, a move reads volume.to_disk.
func TestJobViewLabelsDiskJobs(t *testing.T) {
	for kind, want := range map[string]string{
		"disk-add": "disk.add", "disk-check": "disk.check", "disk-forget": "disk.forget", "disk-adopt": "disk.adopt",
		"disk-add-device": "disk.add_device", "disk-grow": "disk.grow",
		"volume-to-disk": "volume.to_disk", "deploy": "deploy",
	} {
		if got := render.JobView("", service.Job{Kind: kind}).Kind; got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
}

// A failed disk job reads as one plain sentence; the raw error (paths, Docker text) is not in the line.
func TestJobViewWordsFailedDiskJobs(t *testing.T) {
	for _, c := range []struct{ kind, err, want string }{
		{"volume-to-disk", "checking the copy of x: open /mnt/d/x: boom", "The copy failed. The volume is unchanged."},
		{"volume-to-disk", "web did not start on the new copy: boom\napi did not start on the new copy: boom", "web and api did not start on the new copy."},
		{"disk-forget", "remove volume v: Docker said no", "Forgetting did not finish."},
		{"disk-adopt", "x: boom", "Adopting did not finish."},
		{"deploy", "boom", "boom"},
	} {
		if got := render.JobView("", service.Job{Kind: c.kind, State: "failed", Error: c.err}).Error; got != c.want {
			t.Errorf("%s: %q, want %q", c.kind, got, c.want)
		}
	}
	if got := render.JobView("", service.Job{Kind: "disk-forget", State: "running", Error: "x"}).Error; got != "x" {
		t.Errorf("a live job keeps its text: %q", got)
	}
}
