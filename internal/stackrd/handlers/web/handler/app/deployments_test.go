package app

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The image in use is the newest *done* build with a tag, not the newest row:
// after a failed deploy the running image is the one below it.
func TestLiveDeployment(t *testing.T) {
	cases := []struct {
		name string
		in   []repo.Deployment
		want int
	}{
		{"none", nil, -1},
		{"newest is live", []repo.Deployment{
			{Status: "done", ImageTag: "b"}, {Status: "done", ImageTag: "a"},
		}, 0},
		{"skips failed and running", []repo.Deployment{
			{Status: "error"}, {Status: "running"}, {Status: "done", ImageTag: "a"},
		}, 2},
		{"done without a tag is not live", []repo.Deployment{
			{Status: "done"}, {Status: "done", ImageTag: "a"},
		}, 1},
		{"nothing shipped", []repo.Deployment{{Status: "error"}}, -1},
	}
	for _, tc := range cases {
		if got := liveDeployment(tc.in); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
