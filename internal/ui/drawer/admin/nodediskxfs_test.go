package admin

import (
	"strings"
	"testing"
)

// Phase 3 screens of the node drawer (mockups 17 and 18), drawn from
// constructed views: the states the handler tests cannot reach (a worker's
// device facts) and the exact words.

func render3(t *testing.T, d NodeDisk) string {
	t.Helper()
	d.Has, d.Whole, d.Path = true, true, "/var/lib/stackr-disks/dk_7f3a"
	if d.Device == "" {
		d.Device, d.DeviceSize = "sdb", "500 GB"
	}
	return renderC(t, diskSection(d))
}

// 17t, 18a, 18b, 18c: the xfs card says the device, offers Use it only while
// the device has new space, and disables it while the grow runs.
func TestXfsCardGrowRow(t *testing.T) {
	ready := render3(t, NodeDisk{State: "ready", Used: "278 GB of 499 GB", Pct: "56"})
	wants(t, "ready", ready, ">Device</dt>", ">sdb</span>", "500 GB", "Ready", "/var/lib/stackr-disks/dk_7f3a", "278 GB of 499 GB")
	if strings.Contains(ready, "New space") || strings.Contains(ready, "Use it") {
		t.Errorf("a disk without new space offers Use it\n%s", ready)
	}
	noPanel(t, "18c", ready)

	row := render3(t, NodeDisk{State: "ready", Used: "278 GB of 499 GB", Pct: "56", NewSpace: "200 GB", Grow: "/g"})
	wants(t, "18a", row, "New space on the device: 200 GB.", ">Use it</button>", `hx-post="/g"`, "The disk stays in use.")
	noPanel(t, "18a", row)

	run := render3(t, NodeDisk{State: "ready", NewSpace: "200 GB", Grow: "/g", Growing: true})
	wants(t, "18b", run, "New space on the device: 200 GB.", "disabled>Use it</button>")
	if strings.Contains(run, `hx-post="/g"`) {
		t.Error("Use it can be pressed while a grow runs")
	}
}

// 18f, 18g: a device that changed is the danger chip, no Used reading, and the
// strip names what to do; Check disk is the one button.
func TestXfsDamagedCardAndStrip(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"18f", "sdb is now 300 GB, was 500 GB. Data on it may be damaged. Make the device 500 GB again, then check the disk."},
		{"18g", "Another filesystem is on sdb now. The disk&#39;s data may be gone."},
	} {
		d := NodeDisk{State: "ready", Damaged: strings.ReplaceAll(tc.line, "&#39;", "'"), DeviceWas: "500 GB", Check: "/c", Used: "1 GB of 2 GB", Pct: "50"}
		card := render3(t, d)
		wants(t, tc.name, card, "Device changed", ">unknown</dd>")
		if strings.Contains(card, "Ready") || strings.Contains(card, "1 GB of 2 GB") {
			t.Errorf("%s: a damaged disk shows Ready or a Used reading\n%s", tc.name, card)
		}
		noPanel(t, tc.name, card)
		d.Whole, d.Has = true, true
		strip := renderC(t, diskStrip(d))
		wants(t, tc.name, strip, tc.line, "Tiles that use it do not start.", ">Check disk</button>", `hx-post="/c"`)
	}
}

// 17q, 17r, 17s, 17s2: an add in progress says what it does; a failed one
// offers Try again (the picker), Check disk only after the format, and a note
// that a formatted disk is only mounted.
func TestXfsAddingStates(t *testing.T) {
	run := render3(t, NodeDisk{State: "adding", Note: "Formatting sdb."})
	wants(t, "17q", run, "Adding", "Formatting sdb.")
	if strings.Contains(run, "New space") || strings.Contains(run, "Volumes on this disk") {
		t.Errorf("an adding card lists volumes or new space\n%s", run)
	}

	after := NodeDisk{Has: true, Whole: true, State: "adding", AddFailed: true, Retry: "/choose", Check: "/c", Formatted: true}
	strip := renderC(t, diskStrip(after))
	wants(t, "17s", strip, "Adding did not finish.", ">Check disk</button>", ">Try again</button>", `hx-post="/choose"`, `id="disk-retry"`)
	before := after
	before.Check = ""
	strip = renderC(t, diskStrip(before))
	wants(t, "17s2", strip, "Adding did not finish.", ">Try again</button>")
	if strings.Contains(strip, "Check disk") {
		t.Error("17s2: Check disk offered before anything was formatted")
	}
}

func devs3() *DiskPicker {
	return &DiskPicker{Devices: []DiskDevice{
		{Name: "sdb", Model: "Samsung SSD 870 EVO", Size: "500 GB", Ident: "S5Y1NX0R312944A", Value: `{"id":"b"}`, Chip: "No filesystem"},
		{Name: "sdc", Model: "WDC WD20EZAZ", Size: "2 TB", Ident: "WD-WX12D8052961", Value: `{"id":"c"}`, Chip: "ext4 filesystem", Tone: "warn"},
		{Name: "sda", Model: "QEMU HARDDISK", Size: "80 GB", Ident: "drive-scsi0", Value: `{"id":"a"}`, Why: "it holds the system or Docker"},
	}}
}

func dlg3(p *DiskPicker) DiskDialog {
	return DiskDialog{Probe: "/p", State: "/s", Add: "/a", Choose: "/choose", Pick: "/pick", Plan: "/plan", How: "disk", Node: "vm-hel-2", Picker: p}
}

// 17a to 17f: two choices, the list with what is on each disk, blocked disks
// greyed with the reason and no way to pick them, Continue off until a pick.
func TestAddDiskDialogChoiceAndPicker(t *testing.T) {
	d := dlg3(devs3())
	out := renderC(t, diskDialog(d, "Add disk", "btn"))
	wants(t, "17c", out,
		"A folder that is already mounted", "Nothing is erased.", "A whole disk", "stackr formats it. Everything on it is erased.",
		"New volumes on this node are stored on it. Existing volumes stay where they are.",
		"Disks on vm-hel-2", ">Refresh</button>", "Reading the disks on vm-hel-2...",
		"Samsung SSD 870 EVO", "500 GB", "S5Y1NX0R312944A", "No filesystem", "ext4 filesystem",
		"Not available: it holds the system or Docker.", `hx-post="/pick"`, `hx-post="/choose"`)
	i := strings.Index(out, `aria-label="sda"`)
	if i < 0 || !strings.Contains(out[max(0, i-200):i+40], " disabled ") {
		t.Errorf("a blocked disk can be picked\n%s", out)
	}
	if !strings.Contains(out, `id="disk-continue"`) || !strings.Contains(out[strings.Index(out, `id="disk-continue"`):], " disabled>") {
		t.Error("Continue is enabled with no pick")
	}
	if strings.Contains(out, `id="disk-path"`) {
		t.Error("the folder field shows beside the picker")
	}
	noPanel(t, "17c", out)

	d.Picker.Devices[0].Picked = true
	out = renderC(t, diskFoot(d))
	if strings.Contains(out, " disabled>") || !strings.Contains(out, ">Continue</button>") {
		t.Errorf("17d: Continue stays off after a pick\n%s", out)
	}
	// a picked disk that is blocked never enables Continue
	d.Picker.Devices[0].Why = "it is in use (mounted)"
	if out = renderC(t, diskFoot(d)); !strings.Contains(out, " disabled>") {
		t.Error("Continue enabled on a blocked disk")
	}

	// the folder choice is the phase 1 dialog as it was
	d.How = "folder"
	folder := renderC(t, diskDialog(d, "Add disk", "btn"))
	wants(t, "17a", folder, `id="disk-path"`, ">Use folder</button>", "A mounted, empty folder. Nothing is erased.")
}

// 17f: nothing usable, and a refusal of the list.
func TestAddDiskDialogNothingUsableAndRefusal(t *testing.T) {
	p := &DiskPicker{Devices: []DiskDevice{{Name: "sda", Size: "80 GB", Ident: "x", Why: "it holds the system or Docker"}}}
	wants(t, "17f", renderC(t, diskPart(dlg3(p))), "No disk can be used. Attach one, then refresh.")
	out := renderC(t, diskPart(dlg3(&DiskPicker{Msg: "vm-hel-2 did not answer. Try again."})))
	wants(t, "refusal", out, "vm-hel-2 did not answer. Try again.")
	if strings.Contains(out, "No disk can be used") {
		t.Error("a failed read says no disk can be used")
	}
}

// 17g to 17k2: the typed name guards Format; Format anyway is its own wording
// and posts force; nothing else posts it.
func TestFormatDialogs(t *testing.T) {
	base := DiskFormat{Add: "/fmt", Node: "vm-hel-2", Name: "sdb", Model: "Samsung", Size: "500 GB", Serial: "S5Y", ID: "ata-X", Value: `{"id":"b"}`}
	out := renderC(t, DiskFormatDialog(base))
	wants(t, "17g", out, "Format sdb", "vm-hel-2", "Erases sdb and formats it xfs. It becomes this node&#39;s disk.", `word="sdb"`, `data-word`,
		`name="typed"`, `hx-include="find [data-word]"`, ">Format disk</button>", `hx-post="/fmt"`, "S5Y", "ata-X", `btn-danger`)
	if strings.Contains(out, "force") || strings.Contains(out, "Format anyway") {
		t.Errorf("a plain format posts force\n%s", out)
	}
	noPanel(t, "17g", out)

	fs := base
	fs.Anyway, fs.Holds, fs.Name = true, "ext4 filesystem, label backup", "sdc"
	out = renderC(t, DiskFormatDialog(fs))
	wants(t, "17i", out, "Format sdc anyway", "sdc holds data. Formatting erases all of it. It cannot be undone.", "ext4 filesystem, label backup",
		">Format anyway</button>", "&#34;force&#34;:&#34;1&#34;", `word="sdc"`)

	st := base
	st.Anyway, st.Holds, st.Hint = true, "An earlier stackr disk. Its volumes are erased.", "To keep them, adopt the disk instead."
	wants(t, "17k", renderC(t, DiskFormatDialog(st)), "An earlier stackr disk. Its volumes are erased.", "To keep them, adopt the disk instead.")

	tb := base
	tb.Anyway, tb.Table, tb.Holds = true, true, "an empty partition table (gpt)"
	wants(t, "17k2", renderC(t, DiskFormatDialog(tb)), "sdb has a partition table. Formatting erases it. It cannot be undone.", "an empty partition table (gpt)")

	// 17l to 17p: a refusal has no typed field and no Format button, only Close
	rf := base
	rf.Msg = "That disk changed since it was listed. Refresh the list and pick it again."
	out = renderC(t, DiskFormatDialog(rf))
	wants(t, "17l", out, rf.Msg, ">Close</button>")
	if strings.Contains(out, "data-word") || strings.Contains(out, "Format disk") || strings.Contains(out, `hx-post="/fmt"`) {
		t.Errorf("a refused format can be pressed again\n%s", out)
	}
	noPanel(t, "17l", out)
}
