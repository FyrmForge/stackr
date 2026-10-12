package service

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// The live-move helper reaches the registry: nothing when it is there, a
// push of the local copy, a build first when there is none.
func TestRsyncStepsEnsure(t *testing.T) {
	for _, c := range []struct {
		name         string
		inReg, local bool
		regErr       error
		want         []string
		wantErr      bool
	}{
		{name: "in registry", inReg: true},
		{name: "local only", local: true, want: []string{"push"}},
		{name: "nothing", want: []string{"build", "push"}},
		{name: "registry down", regErr: errors.New("down"), wantErr: true},
	} {
		var did []string
		s := rsyncSteps{
			inRegistry: func(context.Context) (bool, error) { return c.inReg, c.regErr },
			local:      func(context.Context) bool { return c.local },
			build:      func(context.Context) error { did = append(did, "build"); return nil },
			push:       func(context.Context) error { did = append(did, "push"); return nil },
		}
		err := s.ensure(context.Background())
		if (err != nil) != c.wantErr || !slices.Equal(did, c.want) {
			t.Errorf("%s: did %v err %v, want %v", c.name, did, err, c.want)
		}
	}
}
