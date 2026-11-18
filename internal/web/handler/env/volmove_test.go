package env

import "testing"

// 15d: the refusal names the job that holds the volume.
func TestMoveRefusalNamesTheJob(t *testing.T) {
	for kind, want := range map[string]string{
		"backup":         "A backup is running. Try again when it is done.",
		"restore":        "A restore is running. Try again when it is done.",
		"volume-to-disk": "A move is running. Try again when it is done.",
		"deploy":         "A deploy is running. Try again when it is done.",
		"seed":           "Another job is running. Try again when it is done.",
	} {
		if got := moveRefusal("a " + kind + " job holds the data volume; waiting for it"); got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
}
