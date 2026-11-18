package admin

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// The service's adopt refusals read in the dialog's words (14f-14n).
func TestAdoptRefusalText(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    service.DiskAdoptPlan
		want string
	}{
		{"14f", service.DiskAdoptPlan{Refusal: "The disk's node, vm-hel-1, is still online. A disk can only be adopted once its node is gone."}, "vm-hel-1 is still online. Forget the node first."},
		{"14g", service.DiskAdoptPlan{Refusal: "The disk's node, vm-hel-1, is offline but not forgotten. Forget the node first: it may still be writing to the disk."}, "vm-hel-1 is offline, not forgotten. Forget the node first."},
		{"14h", service.DiskAdoptPlan{Refusal: "Remove the standbys first.", OldNode: "vm-hel-1", Hosts: []service.AdoptHost{{Env: "shop / prod", Tile: "postgres"}}}, "vm-hel-1 hosts a standby of shop / prod postgres. Remove the standby first."},
		{"14m", service.DiskAdoptPlan{Refusal: "Remove the standbys first.", OldNode: "vm-hel-1", Hosts: []service.AdoptHost{{Env: "shop / prod", Tile: "postgres", Primary: true}}}, "vm-hel-1 runs the primary of shop / prod. Move it back first."},
		{"14i", service.DiskAdoptPlan{Refusal: "This folder carries no disk this panel knows."}, "This folder is not a stackr disk."},
		{"14j", service.DiskAdoptPlan{Refusal: "This node already has a disk."}, "This node already has a disk."},
		{"14n", service.DiskAdoptPlan{Refusal: "This disk is already being adopted for another node or path. Finish that adopt first.", AdoptedBy: "vm-hel-6"}, "This disk is being adopted by vm-hel-6."},
	} {
		if got := adoptRefusalText(tc.p); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
