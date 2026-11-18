package main

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// stkr node disk and stkr volume move-to-disk: the CLI side of a node's
// disk. Every verb mirrors the panel's: the same plan first, the same
// refusals (the server writes them), the same typed word for the destructive
// ones. --confirm <word> answers the question; without it the verb asks. -y
// answers a yes question (adopt) but never a typed word.

// answered is the word the adopt verb sends: the one given with --confirm,
// else the expected one once the person has answered the question.
func (a *app) answered(given, word, q string) (string, error) {
	if given != "" {
		return given, nil
	}
	return word, a.confirm(q)
}

// typed is the word a destructive verb sends: the one given with --confirm, or
// one the person types at the prompt. -y does not skip it, and no terminal
// without --confirm is refused.
func (a *app) typed(given, word, q string) (string, error) {
	if given != "" {
		return given, nil
	}
	if !a.tty || a.json {
		return "", usage("Pass --confirm %s.", word)
	}
	_, _ = fmt.Fprintf(a.errw, "%s Type %s to confirm: ", q, word)
	line, _ := a.in.ReadString('\n')
	return strings.TrimSpace(line), nil
}

// size is a byte count; a negative one is a size the server could not read.
func size(v any) string {
	if num(v) < 0 {
		return "unknown size"
	}
	return gb(v)
}

func confirmFlag(c *cobra.Command, dst *string, what string) {
	c.Flags().StringVar(dst, "confirm", "", "the word that confirms ("+what+"); without it the verb asks")
}

func (a *app) nodeDisk() *cobra.Command {
	return noun("disk", "A node's disk: the folder its volumes live on",
		a.diskLs(), a.diskDevices(), a.diskAdd(), a.diskGrow(), a.diskCheck(), a.diskAdopt(), a.diskForget(), a.diskMoveAll())
}

func nodeDiskPath(node, sub string) string {
	return "/admin/nodes/" + url.PathEscape(node) + "/disk" + sub
}

func (a *app) diskLs() *cobra.Command {
	c := leaf("ls <node>", "admin.node-disk", "Show a node's disk (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			v, err := a.call(GET, nodeDiskPath(args[0], ""), nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			m := asMap(v)
			if m["has"] != true {
				a.say("%s has no disk. Add one: stkr node disk add %s <path>", args[0], args[0])
				return nil
			}
			rows := [][]string{
				{"id", cell(m["id"])}, {"path", cell(m["path"])}, {"kind", cell(m["kind"])}, {"state", cell(m["state"])},
				{"space", fmt.Sprintf("%s free of %s", gb(m["free"]), gb(m["total"]))},
				{"volumes", cell(m["volumes"])},
			}
			if m["stale"] == true {
				rows = append(rows, []string{"read", "not read lately; the numbers may be old"})
			}
			a.table(nil, rows)
			return nil
		})
	c.Example = "  stkr node disk ls vps-fsn1"
	return c
}

// typedWord is a word that must be typed: --confirm gives it, else it is asked
// on a terminal. -y never answers it, because it guards a format.
func (a *app) typedWord(given, word, q string) (string, error) {
	if given != "" {
		return given, nil
	}
	if a.json || !a.tty {
		return "", fmt.Errorf("type %s with --confirm to confirm (--yes does not answer it): %s", word, q)
	}
	_, _ = fmt.Fprintf(a.errw, "%s Type %s to confirm: ", q, word)
	line, _ := a.in.ReadString('\n')
	return strings.TrimSpace(line), nil
}

// devName is a device as the panel names it: sdb for /dev/sdb.
func devName(m map[string]any) string {
	return path.Base(cell(m["path"]))
}

func (a *app) diskDevices() *cobra.Command {
	c := leaf("devices <node>", "admin.node-disk-devices",
		"List the whole disks on the node and why each can or cannot be added (server admin)", exact(1),
		func(_ *cobra.Command, args []string) error {
			v, err := a.call(POST, nodeDiskPath(args[0], "/devices"), nil)
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			var rows [][]string
			for _, x := range asList(asMap(v)["devices"]) {
				m := asMap(x)
				what := "free"
				switch {
				case cell(m["blocked"]) != "(none)":
					what = "not available: " + cell(m["blocked"])
				case cell(m["signature"]) != "(none)":
					what = "holds data: " + cell(m["signature"])
				}
				rows = append(rows, []string{devName(m), gb(m["bytes"]), cell(m["id"]), what})
			}
			if len(rows) == 0 {
				a.say("%s has no whole disks.", args[0])
				return nil
			}
			a.table([]string{"disk", "size", "id", "state"}, rows)
			return nil
		})
	c.Example = "  stkr node disk devices vps-fsn1"
	return c
}

func (a *app) diskAdd() *cobra.Command {
	var device, confirm string
	var anyway bool
	c := waits(leaf("add <node> [<path>]", "admin.node-disk-probe,admin.node-disk-add,admin.node-disk-devices,admin.node-disk-add-device",
		"Make a mounted folder, or with --device a whole disk that is formatted, the node's disk (server admin)", cobra.RangeArgs(1, 2),
		func(c *cobra.Command, args []string) error {
			if device != "" {
				if len(args) != 1 {
					return usage("give a folder or --device, not both")
				}
				return a.addDevice(c, args[0], device, confirm, anyway)
			}
			if len(args) != 2 {
				return usage("give the folder to add, or --device <disk> to format a whole disk")
			}
			v, err := a.call(POST, nodeDiskPath(args[0], "/probe"), map[string]string{"path": args[1]})
			if err != nil {
				return err
			}
			p := asMap(v)
			if m := cell(p["message"]); cell(p["code"]) != "(none)" {
				return errors.New(m)
			}
			a.say("%s: %s, %s free of %s", args[1], cell(p["fs_type"]), gb(p["free"]), gb(p["total"]))
			return a.job(c, "/admin", POST, nodeDiskPath(args[0], ""), map[string]string{"path": args[1]}, "adding the disk")
		}))
	c.Flags().StringVar(&device, "device", "", "a whole disk to format xfs: its name (sdb), path or id from `devices`. Everything on it is erased")
	c.Flags().BoolVar(&anyway, "format-anyway", false, "erase a disk that already holds a filesystem, a partition table or an earlier stackr disk")
	confirmFlag(c, &confirm, "the disk's name, like sdb; --yes does not answer it")
	c.Example = "  stkr node disk add vps-fsn1 /mnt/data\n  stkr node disk add vps-fsn1 --device sdb --confirm sdb"
	return c
}

// addDevice formats the listed disk. The disk goes back whole, as it was
// listed, so one that changed since is refused; the typed name is never
// skipped by -y, and the server decides what needs Format anyway.
func (a *app) addDevice(c *cobra.Command, node, device, confirm string, anyway bool) error {
	v, err := a.call(POST, nodeDiskPath(node, "/devices"), nil)
	if err != nil {
		return err
	}
	var pick map[string]any
	for _, x := range asList(asMap(v)["devices"]) {
		m := asMap(x)
		if device == cell(m["id"]) || device == cell(m["path"]) || device == devName(m) {
			pick = m
			break
		}
	}
	if pick == nil {
		return fmt.Errorf("%s has no disk %q; list them: stkr node disk devices %s", node, device, node)
	}
	name := devName(pick)
	if b := cell(pick["blocked"]); b != "(none)" {
		return fmt.Errorf("%s cannot be used (%s)", name, b)
	}
	if sig := cell(pick["signature"]); sig != "(none)" {
		if !anyway {
			return fmt.Errorf("%s already holds data (%s). Add --format-anyway to erase it", name, sig)
		}
		a.say("%s holds data (%s). Formatting erases all of it.", name, sig)
	}
	word, err := a.typedWord(confirm, name, "Erase "+name+" ("+gb(pick["bytes"])+") and format it xfs?")
	if err != nil {
		return err
	}
	return a.job(c, "/admin", POST, nodeDiskPath(node, "/add-device"),
		map[string]any{"device": pick, "confirm": word, "force": anyway}, "formatting the disk")
}

func (a *app) diskGrow() *cobra.Command {
	return waits(leaf("grow <node>", "admin.node-disk-grow",
		"Grow the node's xfs disk into the space its device gained; the disk stays in use (server admin)", exact(1),
		func(c *cobra.Command, args []string) error {
			return a.job(c, "/admin", POST, nodeDiskPath(args[0], "/grow"), nil, "growing the disk")
		}))
}

func (a *app) diskCheck() *cobra.Command {
	return waits(leaf("check <node>", "admin.node-disk-check",
		"Check the node's disk again; the way out of missing (server admin)", exact(1),
		func(c *cobra.Command, args []string) error {
			return a.job(c, "/admin", POST, nodeDiskPath(args[0], "/check"), nil, "disk check")
		}))
}

// sayAdoptVolumes prints the volumes of one plan list, one line each.
func (a *app) sayAdoptVolumes(head string, vs []any) {
	for _, x := range vs {
		v := asMap(x)
		where, line := cell(v["where"]), head+" "+cell(v["slug"])
		if where == "(none)" {
			where = "orphan"
		}
		line += " (" + where + ")"
		if v["has_backup"] == true {
			line += ", backup " + cell(v["backup"])[:10]
		}
		a.say("%s", line)
	}
}

func (a *app) diskAdopt() *cobra.Command {
	var confirm, discard string
	c := waits(leaf("adopt <node> [<path>]", "admin.node-disk-adopt-plan,admin.node-disk-adopt,admin.node-disk-adopt-discard",
		"Put the disk of a forgotten node on this node, with the envs that lived there. No path lists the disks to adopt (server admin)", cobra.RangeArgs(1, 2),
		func(c *cobra.Command, args []string) error {
			node, path := args[0], ""
			if len(args) == 2 {
				path = args[1]
			}
			v, err := a.call(GET, nodeDiskPath(node, "/adopt-plan?path="+url.QueryEscape(path)), nil)
			if err != nil {
				return err
			}
			p := asMap(v)
			if path == "" {
				if d := cell(p["resume_disk"]); d != "(none)" {
					a.say("An adopt of this node was cut off on %s. Resume it: stkr node disk adopt %s %s", cell(p["resume_path"]), node, cell(p["resume_path"]))
					return nil
				}
				cands := asList(p["candidates"])
				for _, x := range cands {
					m := asMap(x)
					a.say("%s\t%s\t%s\t%s", cell(m["disk_id"]), cell(m["node"]), cell(m["path"]), cell(m["state"]))
				}
				if len(cands) == 0 {
					a.say("No disk of a forgotten node waits to be adopted.")
				} else {
					a.say("Adopt one: stkr node disk adopt %s <path of the mounted disk>", node)
				}
				return nil
			}
			resume := cell(p["resume_disk"]) != "(none)" && cell(p["resume_path"]) == path
			if a.json {
				return a.show(v)
			}
			a.say("Adopt %s on %s: the disk of %s (%s)", path, cell(p["node"]), cell(p["old_node"]), cell(p["disk_id"]))
			for _, e := range asList(p["envs"]) {
				m := asMap(e)
				a.say("  env  %s  (%s)", cell(m["label"]), plural(int(num(m["tiles"])), "tile", "tiles"))
			}
			a.sayAdoptVolumes("  on the disk", asList(p["on_disk"]))
			a.sayAdoptVolumes("  restored or empty", asList(p["elsewhere"]))
			for _, f := range strs(p["unclaimed"]) {
				a.say("  kept, nobody claims it  %s", f)
			}
			for _, x := range asList(p["copies"]) {
				m := asMap(x)
				a.say("  blocks the adopt: a copy of %s on this node. Remove it with --discard %s", cell(m["slug"]), cell(m["slug"]))
			}
			for _, x := range asList(p["hosts"]) {
				m := asMap(x)
				a.say("  blocks the adopt: %s hosts %s", cell(m["env"]), cell(m["tile"]))
			}
			if discard != "" {
				return a.adoptDiscard(node, path, p, discard, confirm)
			}
			if r := cell(p["refusal"]); r != "(none)" {
				return errors.New(r)
			}
			word := confirm
			if !resume {
				if word, err = a.answered(confirm, cell(p["node"]), "Adopt this disk on "+cell(p["node"])+"? The envs above move to it."); err != nil {
					return err
				}
			}
			return a.job(c, "/admin", POST, nodeDiskPath(node, "/adopt"),
				map[string]string{"disk_id": cell(p["disk_id"]), "path": path, "confirm": word}, "adopt")
		}))
	c.Flags().StringVar(&discard, "discard", "", "remove this blocking plain copy (a volume slug) first; the folder stays")
	confirmFlag(c, &confirm, "the node's name, or the volume's slug with --discard")
	c.Example = "  stkr node disk adopt vps-new\n  stkr node disk adopt vps-new /mnt/data"
	return c
}

// adoptDiscard removes the plain copy of the volume slug that blocks an adopt.
func (a *app) adoptDiscard(node, path string, p map[string]any, slug, confirm string) error {
	for _, x := range asList(p["copies"]) {
		m := asMap(x)
		if cell(m["slug"]) != slug && cell(m["volume_id"]) != slug {
			continue
		}
		slug = cell(m["slug"])
		word, err := a.typed(confirm, slug, "Remove the plain copy of "+slug+" on "+node+"? The folder on the disk stays.")
		if err != nil {
			return err
		}
		if _, err := a.call(POST, nodeDiskPath(node, "/adopt-discard"), map[string]string{
			"disk_id": cell(p["disk_id"]), "path": path, "volume_id": cell(m["volume_id"]), "confirm": word}); err != nil {
			return err
		}
		a.say("Removed the copy of %s. Run the adopt again.", slug)
		return nil
	}
	return fmt.Errorf("no blocking copy of %q on %s", slug, node)
}

func (a *app) diskForget() *cobra.Command {
	var confirm string
	c := waits(leaf("forget <disk-id>", "admin.node-disk-forget-plan,admin.node-disk-forget",
		"Forget a disk that is missing, half added or left by a forgotten node; its envs are restored from backups (server admin)", exact(1),
		func(c *cobra.Command, args []string) error {
			v, err := a.call(GET, "/admin/node-disks/"+url.PathEscape(args[0])+"/forget-plan", nil)
			if err != nil {
				return err
			}
			p := asMap(v)
			if a.json {
				return a.show(v)
			}
			a.say("Forget disk %s of %s", cell(p["disk_id"]), cell(p["node"]))
			if p["gone"] == true {
				a.say("  its node is forgotten: only the record goes")
			}
			a.sayAdoptVolumes("  volume", asList(p["volumes"]))
			if r := cell(p["refusal"]); r != "(none)" {
				return errors.New(r)
			}
			word := cell(p["node"])
			if word == "(none)" {
				word = cell(p["disk_id"])
			}
			if word, err = a.typed(confirm, word, "Forget this disk? Its containers and Docker objects are removed and each env is restored from its backups. The folders on the disk stay."); err != nil {
				return err
			}
			j, err := a.call(POST, "/admin/node-disks/"+url.PathEscape(args[0])+"/forget", map[string]string{"confirm": word})
			if err != nil {
				return err
			}
			if cell(asMap(j)["id"]) == "(none)" {
				a.say("Disk forgotten.")
				return nil
			}
			if nw, _ := c.Flags().GetBool("no-wait"); nw {
				return a.show(j, "id", "kind", "state")
			}
			return a.follow("/admin", j, "forgetting the disk")
		}))
	confirmFlag(c, &confirm, "the node's name or the disk's id")
	c.Example = "  stkr node disk forget 3f2a9c1e"
	return c
}

func (a *app) diskMoveAll() *cobra.Command {
	c := waits(leaf("move-all <node>", "admin.node-disk-move-all-plan,admin.node-disk-move-all",
		"Copy every volume of the node onto its disk, one env at a time, smallest first (server admin)", exact(1),
		func(c *cobra.Command, args []string) error {
			v, err := a.call(GET, nodeDiskPath(args[0], "/move-all-plan"), nil)
			if err != nil {
				return err
			}
			p := asMap(v)
			if a.json {
				return a.show(v)
			}
			for _, g := range asList(p["groups"]) {
				m := asMap(g)
				a.say("move  %s (%s), tiles restart: %s", volumeSlugs(m["volumes"]), size(m["size_bytes"]), strings.Join(strs(m["tiles"]), ", "))
			}
			for _, x := range asList(p["skipped"]) {
				m := asMap(x)
				a.say("skip  %s (%s): %s", cell(m["slug"]), cell(m["env"]), cell(m["reason"]))
			}
			switch {
			case p["busy"] == true:
				return errors.New("volumes are already moving to the disk")
			case len(asList(p["groups"])) == 0:
				a.say("Nothing to move.")
				return nil
			case p["fits"] != true:
				return fmt.Errorf("the disk has %s free and the moves need %s (the data and a tenth over)", gb(p["free"]), gb(p["need"]))
			}
			if err := a.confirm(fmt.Sprintf("Move these volumes to the disk of %s? Each env's tiles stop while its volumes copy.", args[0])); err != nil {
				return err
			}
			r, err := a.call(POST, nodeDiskPath(args[0], "/move-all"), nil)
			if err != nil {
				return err
			}
			if nw, _ := c.Flags().GetBool("no-wait"); nw {
				return a.show(r)
			}
			for _, j := range asList(asMap(r)["jobs"]) {
				if err := a.follow("/admin", j, "move to disk"); err != nil {
					return err
				}
			}
			if m := cell(asMap(r)["refused"]); m != "(none)" {
				return errors.New("some groups were not queued: " + m)
			}
			return nil
		}))
	c.Example = "  stkr node disk move-all vps-fsn1"
	return c
}

func volumeSlugs(v any) string {
	var s []string
	for _, x := range asList(v) {
		s = append(s, cell(asMap(x)["slug"]))
	}
	return strings.Join(s, ", ")
}

// volumeMoveToDisk is `stkr volume move-to-disk`.
func (a *app) volumeMoveToDisk() *cobra.Command {
	return waits(leaf("move-to-disk <volume>", "volume.move-plan,volume.move-to-disk",
		"Copy a volume onto its node's disk; the tiles that mount it stop while it copies", exact(1),
		a.at(atOrg, func(c *cobra.Command, p string, args []string) error {
			id, name, err := a.volume(c, args[0])
			if err != nil {
				return err
			}
			v, err := a.call(GET, p+"/volumes/"+id+"/move-plan", nil)
			if err != nil {
				return err
			}
			m := asMap(v)
			if a.json {
				return a.show(v)
			}
			if m["finish"] == true {
				a.say("%s (%s): an earlier move was cut off; only the deploy is left", cell(m["slug"]), cell(m["env"]))
			} else {
				a.say("%s (%s): %s, tiles stop while it copies: %s", cell(m["slug"]), cell(m["env"]), size(m["size_bytes"]), strings.Join(strs(m["tiles"]), ", "))
				if m["fits"] != true {
					return fmt.Errorf("the disk has %s free and the move needs %s (the data and a tenth over)", gb(m["free"]), gb(m["need"]))
				}
			}
			if err := a.confirm("Move volume " + name + " to the disk? Its tiles stop while it copies."); err != nil {
				return err
			}
			return a.job(c, p, POST, p+"/volumes/"+id+"/move-to-disk", nil, "move to disk")
		})))
}

// capMB reads a size cap: 20GB, 1500MB, 2TB, or none (null). A bare number is MB.
func capMB(s string) (*int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "none" || s == "" {
		return nil, nil
	}
	mult := int64(1)
	for suf, m := range map[string]int64{"tb": 1 << 20, "gb": 1 << 10, "mb": 1} {
		if strings.HasSuffix(s, suf) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, suf)), m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return nil, usage("give a size like 20GB or 1500MB, or none")
	}
	if n > (1<<30)/mult {
		return nil, usage("give a size up to 1 PB")
	}
	mb := n * mult
	return &mb, nil
}

// volumeCap is `stkr volume cap`.
func (a *app) volumeCap() *cobra.Command {
	c := leaf("cap <volume> <size|none>", "volume.cap",
		"Set a volume's size cap, like 20GB, or remove it with none. Only a volume on a whole disk is held to it", exact(2),
		a.at(atOrg, func(c *cobra.Command, p string, args []string) error {
			id, _, err := a.volume(c, args[0])
			if err != nil {
				return err
			}
			mb, err := capMB(args[1])
			if err != nil {
				return err
			}
			v, err := a.call(PUT, p+"/volumes/"+id+"/cap", map[string]*int64{"max_size_mb": mb})
			if err != nil {
				return err
			}
			if a.json {
				return a.show(v)
			}
			if asMap(v)["enforced"] != true {
				a.say("Saved. It is not enforced: only a volume on a whole disk is held to it.")
			}
			return nil
		}))
	c.Example = "  stkr volume cap pgdata 20GB\n  stkr volume cap pgdata none"
	return c
}

// volumeDiscard is `stkr volume discard`: the ghost's resolutions.
func (a *app) volumeDiscard() *cobra.Command {
	var bound bool
	var confirm string
	c := leaf("discard <volume>", "volume.discard-plain,volume.discard-bound",
		"Remove the plain Docker copy of a volume that also has a disk folder; --bound removes an object bound elsewhere. The folders stay", exact(1),
		a.at(atOrg, func(c *cobra.Command, p string, args []string) error {
			id, name, err := a.volume(c, args[0])
			if err != nil {
				return err
			}
			slug, _, _ := strings.Cut(name, " (")
			op := "/discard-plain"
			if bound {
				op = "/discard-bound"
			}
			word, err := a.typed(confirm, slug, "Remove the Docker object of volume "+name+"? The folders on the disk stay.")
			if err != nil {
				return err
			}
			_, err = a.call(POST, p+"/volumes/"+id+op, map[string]string{"confirm": word})
			return err
		}))
	c.Flags().BoolVar(&bound, "bound", false, "the object is bound somewhere other than the volume's folder")
	confirmFlag(c, &confirm, "the volume's slug")
	return c
}
