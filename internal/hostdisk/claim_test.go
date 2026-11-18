package hostdisk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func claimReq(q *Request) { q.Owner, q.From = "n2", "n1" }

func (r *rig) stampOwner() string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.disk, stampFile))
	if err != nil {
		r.t.Fatal(err)
	}
	var s Stamp
	if err := json.Unmarshal(b, &s); err != nil {
		r.t.Fatal(err)
	}
	return s.Owner
}

// DL: the claim rewrites the owner; the old node's verify then fails with
// foreign_stamp, the new node's passes, and a repeat is accepted.
func TestClaimRewritesTheOwnerAndTheOldOwnerIsForeign(t *testing.T) {
	r := added(t)
	wantCode(t, r.run("claim", claimReq), "")
	if r.stampOwner() != "n2" {
		t.Fatalf("owner = %s", r.stampOwner())
	}
	wantCode(t, r.run("verify", nil), CodeForeignStamp)
	wantCode(t, r.run("verify", func(q *Request) { q.Owner = "n2" }), "")
	wantCode(t, r.run("claim", claimReq), "")
	if r.exists(stampTmp) {
		t.Error("a stamp temp is left")
	}
}

// DL: a stamp of another disk or a third owner is never taken over.
func TestClaimRefusesAnotherDiskOrAThirdOwner(t *testing.T) {
	r := added(t)
	wantCode(t, r.run("claim", func(q *Request) { claimReq(q); q.DiskID = "d2" }), CodeForeignStamp)
	wantCode(t, r.run("claim", func(q *Request) { q.Owner, q.From = "n2", "n3" }), CodeForeignStamp)
	if r.stampOwner() != "n1" {
		t.Fatalf("owner = %s", r.stampOwner())
	}
	if err := os.Remove(filepath.Join(r.disk, stampFile)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("claim", claimReq), CodeForeignStamp)
	r.write(stampFile, "")
	if res := r.run("claim", claimReq); res.Error == nil || res.Error.Code == "" {
		t.Fatalf("broken stamp: %+v", res.Error)
	}
}

// DL: the mount checks of the other ops hold for claim.
func TestClaimKeepsTheMountChecks(t *testing.T) {
	r := added(t)
	r.table("36 25 8:17 / /disk ro - ext4 /dev/sdb ro\n")
	wantCode(t, r.run("claim", claimReq), CodeReadOnly)
	r.table(rootLine)
	wantCode(t, r.run("claim", claimReq), CodeNotMountPoint)
	wantCode(t, r.run("claim", func(q *Request) { claimReq(q); q.Mount = "/gone" }), CodeDiskMissing)
	r.table(rootLine + diskLine)
	wantCode(t, r.run("claim", func(q *Request) { claimReq(q); q.From = "" }), CodeBadName)
	// the id folder is gone: ENOENT is disk_missing and the stamp is untouched
	if err := os.RemoveAll(filepath.Join(r.disk, "d1")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.run("claim", claimReq), CodeDiskMissing)
	if r.stampOwner() != "n1" {
		t.Fatal("the stamp changed")
	}
}
