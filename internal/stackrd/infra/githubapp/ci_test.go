package githubapp

import "testing"

func TestMergeVerdict(t *testing.T) {
	cases := []struct {
		name     string
		runs     []checkRun
		statuses []commitStatus
		want     CIVerdict
	}{
		{"nothing at all", nil, nil, CINone},
		{"all runs green", []checkRun{{Status: "completed", Conclusion: "success"}}, nil, CIPassing},
		{"neutral and skipped pass", []checkRun{
			{Status: "completed", Conclusion: "neutral"},
			{Status: "completed", Conclusion: "skipped"},
		}, nil, CIPassing},
		{"one run still going", []checkRun{
			{Status: "completed", Conclusion: "success"},
			{Status: "in_progress"},
		}, nil, CIPending},
		{"queued run pends", []checkRun{{Status: "queued"}}, nil, CIPending},
		{"a failed run fails even beside a pending one", []checkRun{
			{Status: "in_progress"},
			{Name: "test", Status: "completed", Conclusion: "failure"},
		}, nil, CIFailing},
		{"timed_out fails", []checkRun{{Status: "completed", Conclusion: "timed_out"}}, nil, CIFailing},
		{"statuses alone pass", nil, []commitStatus{{State: "success"}}, CIPassing},
		{"status error fails", nil, []commitStatus{{Context: "ci/x", State: "error"}}, CIFailing},
		{"status pending pends", nil, []commitStatus{{State: "pending"}}, CIPending},
		{"green runs + red status fails", []checkRun{
			{Status: "completed", Conclusion: "success"},
		}, []commitStatus{{Context: "lint", State: "failure"}}, CIFailing},
		{"green runs + pending status pends", []checkRun{
			{Status: "completed", Conclusion: "success"},
		}, []commitStatus{{State: "pending"}}, CIPending},
	}
	for _, c := range cases {
		if got, _ := mergeVerdict(c.runs, c.statuses); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRepoFull(t *testing.T) {
	cases := map[string]string{
		"https://github.com/owner/repo.git": "owner/repo",
		"https://github.com/owner/repo":     "owner/repo",
		"git@github.com:owner/repo.git":     "owner/repo",
		"https://gitlab.com/owner/repo":     "",
		"":                                  "",
	}
	for in, want := range cases {
		if got := RepoFull(in); got != want {
			t.Errorf("RepoFull(%q) = %q, want %q", in, got, want)
		}
	}
}
