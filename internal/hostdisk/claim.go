package hostdisk

import "errors"

// claim hands the disk to a new owner: the stamp must carry this disk's id
// and an owner that is r.From (the old node) or already r.Owner (a retry),
// and then r.Owner is written the way add writes a stamp (temp file, sync,
// rename, directory sync). Every check of the other ops holds: a real mount
// point that is not the root filesystem, no symlink reaches out of it, the
// id folder is a real folder. Only a missing path is disk_missing; a stamp
// that is not this disk's, or is owned by a third node, is foreign_stamp.
func claim(e Env, r Request) Result {
	if herr := badName(r.DiskID); herr != nil {
		return Result{Error: herr}
	}
	if r.Owner == "" || r.From == "" {
		return fail(CodeBadName, "no owner")
	}
	t, herr := e.open(r.Mount, CodeNotMountPoint)
	if herr != nil {
		return Result{Error: herr}
	}
	defer t.close()
	if herr := t.canonical(); herr != nil {
		return Result{Error: herr}
	}
	if t.readOnly() {
		return fail(CodeReadOnly, r.Mount+" is mounted read-only")
	}
	s, found, err := readStamp(t.root)
	switch {
	case err != nil:
		return Result{Error: stampErr(err, CodeIO)}
	case !found || s.ID != r.DiskID:
		return fail(CodeForeignStamp, "the disk carries another stamp")
	case s.Owner != r.From && s.Owner != r.Owner:
		return fail(CodeForeignStamp, "the disk belongs to another node")
	}
	if herr := realDir(t.root, r.DiskID, CodeDiskMissing); herr != nil {
		return Result{Error: herr}
	}
	if s.Owner != r.Owner {
		s.Owner = r.Owner
		if err := writeStamp(t.root, s, true); err != nil {
			var herr *Error
			if errors.As(err, &herr) {
				return Result{Error: herr}
			}
			return fail(CodeIO, "write the stamp: "+err.Error())
		}
	}
	return Result{Disk: t.fact(s, false)}
}
