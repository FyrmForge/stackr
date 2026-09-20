package installspec

import (
	"strings"
	"testing"
)

func TestCheckRootRefusesWhatIsNotADomain(t *testing.T) {
	for _, in := range []string{"", "localhost", "127.0.0.1", "::1", "example", "ex ample.com", "-bad.com", "a..b.com", "example.123"} {
		if got, err := CheckRoot(in); err == nil {
			t.Errorf("CheckRoot(%q) = %q, want an error", in, got)
		}
	}
}

func TestCheckRootCleansAndKeepsWildcards(t *testing.T) {
	for in, want := range map[string]string{
		"https://Example.COM/path": "example.com",
		"example.com.":             "example.com",
		"example.com:8443":         "example.com",
		"*.apps.example.com":       "*.apps.example.com",
	} {
		got, err := CheckRoot(in)
		if err != nil || got != want {
			t.Errorf("CheckRoot(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// The spec's reason for existing: every env var the installer sets has to be
// in the list the upgrade rewrites, or it reaches fresh installs only.
func TestCreateArgsCarriesTheImageAndTheDataDir(t *testing.T) {
	args := strings.Join(CreateArgs("stkr:v1", Input{DataDir: "/var/lib/stackr", Root: "example.com"}), " ")
	for _, want := range []string{
		"--name " + ServiceName,
		"--env STACKR_IMAGE=stkr:v1",
		"--env DATABASE_PATH=/var/lib/stackr/stackr.db",
		"--env ROOT_DOMAIN=example.com",
		"--mount type=bind,src=/var/lib/stackr,dst=/var/lib/stackr",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("CreateArgs missing %q in: %s", want, args)
		}
	}
}
