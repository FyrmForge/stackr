package volume_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var (
	ctx = context.Background()
	env = volume.Scope{Kind: "env", ID: "e1"}
)

// Orphaning keeps row and data; declaring the same slug re-adopts the same
// volume; another scope gets its own.
func TestOrphanReadopt(t *testing.T) {
	st := servicetest.Store(t)
	l := volume.New(st.Volumes, dockerfake.New())

	v, adopted, err := l.Declare(ctx, env, "pgdata", 0, nil)
	if err != nil || adopted {
		t.Fatalf("declare = %+v, %v, %v", v, adopted, err)
	}
	if _, _, err := l.Declare(ctx, env, "Bad Name", 0, nil); !isInvalid(err) {
		t.Errorf("bad slug = %v", err)
	}
	o, err := l.Orphan(ctx, v)
	if err != nil || o.OrphanedAt == nil {
		t.Fatalf("orphan = %+v, %v", o, err)
	}
	first := *o.OrphanedAt
	if o, _ = l.Orphan(ctx, o); !o.OrphanedAt.Equal(first) {
		t.Error("orphaning twice moved the retention clock")
	}

	if exp, _ := l.Expired(ctx, 30*24*time.Hour, time.Now()); len(exp) != 0 {
		t.Errorf("fresh orphan expired: %v", exp)
	}
	if exp, _ := l.Expired(ctx, 30*24*time.Hour, time.Now().Add(31*24*time.Hour)); len(exp) != 1 {
		t.Errorf("old orphan not expired: %v", exp)
	}

	back, adopted, err := l.Declare(ctx, env, "pgdata", 512, nil)
	if err != nil || !adopted || back.ID != v.ID || back.Name != v.Name || back.OrphanedAt != nil ||
		back.MaxSizeMB != 512 {
		t.Fatalf("re-adopt = %+v, %v, %v", back, adopted, err)
	}
	other, _, _ := l.Declare(ctx, volume.Scope{Kind: "env", ID: "e2"}, "pgdata", 0, nil)
	if other.ID == v.ID {
		t.Error("another env adopted this env's volume")
	}
}

func TestDeleteAndHoldsData(t *testing.T) {
	st := servicetest.Store(t)
	fake := dockerfake.New()
	l := volume.New(st.Volumes, fake)
	v, _, _ := l.Declare(ctx, env, "data", 0, nil)
	if name, err := l.Ensure(ctx, v); err != nil || name != v.Name {
		t.Fatalf("ensure = %s, %v", name, err)
	}

	for _, c := range []struct {
		size int64
		err  error
		want bool
	}{
		{0, nil, false},
		{10, nil, true},
		{-1, nil, true},
		{0, errors.New("daemon down"), true},
		{0, docker.ErrNotFound, false},
	} {
		fake.Volumes = []docker.VolumeInfo{{Name: v.Name, SizeBytes: c.size}}
		fake.Err = map[string]error{"InspectVolume": c.err}
		if got := l.HoldsData(ctx, v); got != c.want {
			t.Errorf("size %d err %v: HoldsData = %v", c.size, c.err, got)
		}
	}

	if err := l.Delete(ctx, v, []string{"db"}); !isConflict(err) {
		t.Errorf("delete while mounted = %v", err)
	}
	fake.Err = map[string]error{"RemoveVolume": errors.New("in use")}
	if err := l.Delete(ctx, v, nil); err == nil {
		t.Error("docker refusal ignored")
	}
	if _, err := l.Get(ctx, v.ID); err != nil {
		t.Error("row gone although docker kept the volume")
	}
	fake.Err = nil
	if err := l.Delete(ctx, v, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Get(ctx, v.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("row after delete: %v", err)
	}
}

func isInvalid(err error) bool {
	_, ok := errs.IsInvalid(err)
	return ok
}

func isConflict(err error) bool {
	_, ok := errs.IsConflict(err)
	return ok
}
