package service

import (
	"cmp"
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// PGVersion is what a postgres instance runs and what stackr offers it.
type PGVersion struct {
	Major    int    `json:"major"`
	Image    string `json:"image"`
	UpdateTo string `json:"update_to"` // the image a minor update moves it to; "" = it is current
	EOL      bool   `json:"eol"`       // its major no longer gets security fixes
	EOLDate  string `json:"eol_date"`  // YYYY-MM-DD, "" = unknown major
}

// PGMajors are the postgres majors a new instance may pick, oldest first;
// the last is the default.
func PGMajors() []int { return mflow.PGPickable(time.Now()) }

// PGVersion is the instance's version, whether a newer minor of its major is
// out and whether its major is past its end of life.
func (o *Orchestrator) PGVersion(ctx context.Context, tileID string) (PGVersion, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PGVersion{}, err
	}
	if m.Engine != "postgres" {
		return PGVersion{}, errs.Invalidf("engine", "%s has no version to update; only postgres does", m.Engine)
	}
	return pgVersionOf(cmp.Or(t.ImageRef, mflow.Engines["postgres"].Definition().Image), m.Major, time.Now()), nil
}

func pgVersionOf(image string, major int, now time.Time) PGVersion {
	v := PGVersion{Major: major, Image: image, EOL: major > 0 && mflow.PGEOL(major, now)}
	v.UpdateTo, _ = mflow.PGUpdate(image)
	if r, ok := mflow.PGReleaseOf(major); ok {
		v.EOLDate = r.EOL.Format(time.DateOnly)
	}
	return v
}

// UpdatePGVersion moves the instance to the current minor of its own major
// and restarts it there. A major change is refused at the tile edit.
func (o *Orchestrator) UpdatePGVersion(ctx context.Context, tileID string) (Tile, *Job, error) {
	v, err := o.PGVersion(ctx, tileID)
	if err != nil {
		return Tile{}, nil, err
	}
	if v.UpdateTo == "" {
		return Tile{}, nil, errs.Conflictf("already on the newest postgres %d minor stackr offers", v.Major)
	}
	return o.UpdateTile(ctx, tileID, func(t *Tile) error {
		t.ImageRef = v.UpdateTo
		return nil
	})
}
