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
		"volume-to-disk": "volume.to_disk", "deploy": "deploy",
	} {
		if got := render.JobView("", service.Job{Kind: kind}).Kind; got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
}
