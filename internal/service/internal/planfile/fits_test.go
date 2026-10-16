package planfile_test

import (
	"reflect"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

func TestFits(t *testing.T) {
	for _, c := range []struct {
		tags []string
		n    planfile.Needs
		want []string
	}{
		{nil, planfile.Needs{}, nil},
		{nil, planfile.Needs{Storage: true}, []string{"needs storage"}},
		{[]string{"storage"}, planfile.Needs{Storage: true}, nil},
		{[]string{"egress"}, planfile.Needs{Storage: true, Ingress: true, Egress: true}, []string{"needs storage", "needs ingress"}},
		{[]string{"ingress", "egress", "storage"}, planfile.Needs{Storage: true, Ingress: true, Egress: true}, nil},
	} {
		if got := planfile.Fits(c.tags, c.n); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Fits(%v, %+v) = %v, want %v", c.tags, c.n, got, c.want)
		}
	}
}
