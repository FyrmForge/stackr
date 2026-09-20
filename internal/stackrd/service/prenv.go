package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// PREnvService owns a stack's pull-request environment settings.
//
// Two things were wrong. `comment` and `status` are keys the config file
// writes — the PR-open hook copies the file's choice onto the stored config,
// because that is what the deploy feedback reads — and neither surface gated
// a panel or API edit to them, so the edit survived until the next pull
// request quietly put the file's value back. And the API's PUT replaced
// `enabled` whether or not the caller sent it, which is why the CLI
// read-merge-writes around it.
type PREnvService struct {
	store repo.Store
	gate  *GateService
}

func NewPREnvService(store repo.Store, gate *GateService) *PREnvService {
	return &PREnvService{store: store, gate: gate}
}

// Get reads the stored settings. The zero value is "not set up".
func (s *PREnvService) Get(ctx context.Context, stackID string) repo.PRConfig {
	return repo.LoadPRConfig(ctx, s.store, stackID)
}

// PREnvPatch is a sparse edit. A nil field is left alone.
//
// Sparse is an explicit exception to D6 ("services take full structs"). D6
// exists to kill the eleven copies of the *tile* field list, where a sparse
// patch means a forgotten field; this is a four-key settings blob with a
// rotate-secret side channel, so the reason for D6 does not apply — and a
// full struct is what made the CLI read-merge-write.
type PREnvPatch struct {
	Enabled *bool
	Comment *bool
	Status  *bool
	// RotateSecret mints a new webhook secret. The old one stops validating
	// at once, so GitHub's webhook has to be updated after.
	RotateSecret bool
}

// Update writes the settings. `comment` and `status` are file-owned on a
// config-managed stack, so editing either goes through the gate; `enabled`
// and the secret are the panel's on any stack — the file can decline to build
// a preview, but the webhook credential is not its business.
func (s *PREnvService) Update(ctx context.Context, stack *repo.Stack, in PREnvPatch, by Actor) (repo.PRConfig, error) {
	if stack == nil {
		return repo.PRConfig{}, svcerr.ErrNotFound
	}
	cfg := repo.LoadPRConfig(ctx, s.store, stack.ID)
	if in.Comment != nil || in.Status != nil {
		if _, err := s.gate.Gate(ctx, stack.ID, GateFieldEdit, by.Surface()); err != nil {
			return cfg, err
		}
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.Comment != nil {
		cfg.NoComment = !*in.Comment
	}
	if in.Status != nil {
		cfg.NoStatus = !*in.Status
	}
	// A stack with PR environments on and no secret cannot validate a
	// webhook at all, so the first save mints one whether or not it was asked.
	if in.RotateSecret || cfg.Secret == "" {
		cfg.Secret = secrets.RandomHex(24)
	}
	return cfg, repo.SavePRConfig(ctx, s.store, stack.ID, cfg)
}

// Adopt is Update without the gate: the config file's own write, at PR-open,
// of the keys it declares. It is what makes the file's choice take effect —
// the deploy feedback reads the stored config, not the file.
func (s *PREnvService) Adopt(ctx context.Context, stackID string, in PREnvPatch) (repo.PRConfig, error) {
	cfg := repo.LoadPRConfig(ctx, s.store, stackID)
	want := cfg
	if in.Enabled != nil {
		want.Enabled = *in.Enabled
	}
	if in.Comment != nil {
		want.NoComment = !*in.Comment
	}
	if in.Status != nil {
		want.NoStatus = !*in.Status
	}
	if want == cfg {
		return cfg, nil
	}
	return want, repo.SavePRConfig(ctx, s.store, stackID, want)
}
