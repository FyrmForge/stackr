package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// valStore answers only what Validate needs for a tile with no connector and
// no storage attachments. Anything else panics, which is the assertion that
// these rules are decided without extra lookups.
type valStore struct{ repo.Store }

func valSvc() *TileService {
	st := valStore{}
	return NewTileService(st, nil, nil, nil, nil, nil, NewGateService(st))
}

// Every case here is a value one surface refused and the other silently
// coerced to a default. SP1: the refusal wins, and it names the field.
func TestValidateRefusesWhatWasSilentlyCoerced(t *testing.T) {
	cases := []struct {
		name  string
		tile  *repo.Tile
		field string
	}{
		{"negative cpu", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", CPULimit: -1}, "limits.cpu"},
		{"negative memory", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", MemLimitMB: -1}, "limits.memory_mb"},
		{"negative shm", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", ShmSizeMB: -1}, "shm_size_mb"},
		{"negative healthcheck knob", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", HealthcheckRetries: -1}, "healthcheck"},
		{"negative timeout", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", TimeoutMinutes: -1}, "timeout_minutes"},
		{"bad update policy", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", UpdatePolicy: "sometimes"}, "update_policy"},
		{"watching a git source", &repo.Tile{Kind: "service", SourceType: "git", GitURL: "https://github.com/a/b", UpdatePolicy: "auto"}, "update_policy"},
		{"ci gate on an image source", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", WaitForCI: true}, "wait_for_ci"},
		{"port on a cron", &repo.Tile{Kind: "cron", SourceType: "image", ImageRef: "x", Cron: "0 3 * * *", ContainerPort: 80}, "port"},
		{"bad device line", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", Devices: "kmsg"}, "devices"},
		{"bad depends_on condition", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", DependsOn: "db:maybe"}, "depends_on"},
		{"bad restart policy", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", RestartPolicy: "sometimes"}, "restart"},
		{"auth user without a password", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", BasicAuthUser: "ops"}, "basic_auth_password"},
		{"image source with no image", &repo.Tile{Kind: "service", SourceType: "image"}, "image"},
		{"git source with no url", &repo.Tile{Kind: "service", SourceType: "git"}, "git_url"},
		{"git source with a bad url", &repo.Tile{Kind: "service", SourceType: "git", GitURL: "ftp://x"}, "git_url"},
		{"unparseable cron", &repo.Tile{Kind: "cron", SourceType: "image", ImageRef: "x", Cron: "not a cron"}, "schedule"},
		{"schedule on a service", &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "x", Cron: "0 3 * * *"}, "schedule"},
		{"privileged on a cron", &repo.Tile{Kind: "cron", SourceType: "image", ImageRef: "x", Cron: "0 3 * * *", Privileged: true}, "privileged"},
		{"run_on_deploy on a cron", &repo.Tile{Kind: "cron", SourceType: "image", ImageRef: "x", Cron: "0 3 * * *", RunOnDeploy: true}, "run_on_deploy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := valSvc().Validate(context.Background(), c.tile)
			inv, ok := svcerr.IsInvalid(err)
			if !ok {
				t.Fatalf("want a 400, got %v (%T)", err, err)
			}
			if inv.Field != c.field {
				t.Errorf("blamed %q, want %q (%s)", inv.Field, c.field, inv.Msg)
			}
		})
	}
}

// A plain, valid service must pass without touching anything but itself, and
// an unset update_policy canonicalises rather than staying ambiguous.
// The upper bound the panel used to clamp to is deliberately not a rule: no
// other surface had one, and a config apply does not run this validator, so
// enforcing it here would refuse a file that applies today.
func TestValidateDoesNotCapTheTimeout(t *testing.T) {
	tile := &repo.Tile{Kind: "cron", SourceType: "image", ImageRef: "x",
		Cron: "0 3 * * *", TimeoutMinutes: 5000}
	if err := valSvc().Validate(context.Background(), tile); err != nil {
		t.Errorf("a long timeout is not an error: %v", err)
	}
}

func TestValidateAcceptsAPlainService(t *testing.T) {
	tile := &repo.Tile{Kind: "service", SourceType: "image", ImageRef: "nginx:1", ContainerPort: 8080}
	if err := valSvc().Validate(context.Background(), tile); err != nil {
		t.Fatalf("a plain service was refused: %v", err)
	}
	if tile.UpdatePolicy != "off" {
		t.Errorf("update_policy = %q, want the canonical \"off\"", tile.UpdatePolicy)
	}
}
