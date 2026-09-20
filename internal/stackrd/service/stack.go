package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// StackOps is the half of a stack's teardown that lives below this package:
// stopping every tile's services and handing the pooled overlay networks
// back by name. An interface because config/envops imports this package.
type StackOps interface {
	TeardownStack(ctx context.Context, stack *repo.Stack) error
}

// StackRenamer renames a stack the whole way. Swarm cannot rename a service
// and every container name derives from the stack slug, so the tiles have to
// be stopped under the old name and redeployed under the new one. Writing
// the slug alone left them serving under a name nothing resolved any more.
// Implemented by config/stackconf.Applier.
type StackRenamer interface {
	RenameStackNow(ctx context.Context, stack *repo.Stack, name string) error
}

// StackService owns the stack row: creating one, renaming it, deleting it,
// moving it between orgs, and binding it to a config repository.
//
// Three of those had no single implementation. Create pre-checked neither an
// empty name nor a duplicate slug on either surface, so two stacks with the
// same name in one org surfaced as a raw UNIQUE 500. Delete tore the stack
// down without reloading the scheduler on either surface, so the cron and
// backup tables kept firing at rows the cascade had already removed. And the
// binding written by a config apply skipped the connector check, the repo
// normalisation and the staged-row drop that the panel's own binding does.
type StackService struct {
	store   repo.Store
	ops     StackOps
	sched   *scheduler.Service
	envs    *EnvironmentService
	renamer StackRenamer
	gate    *GateService
	revoke  *RevokeService
}

func NewStackService(store repo.Store, ops StackOps, sched *scheduler.Service,
	envs *EnvironmentService, renamer StackRenamer, gate *GateService,
	revoke *RevokeService) *StackService {
	return &StackService{store: store, ops: ops, sched: sched, envs: envs,
		renamer: renamer, gate: gate, revoke: revoke}
}

// CreateStack is what a caller asks for.
type CreateStack struct {
	Name        string
	Description string
	// OrgDeclared marks a stack an org config file owns. Only orgconf sets it.
	OrgDeclared bool
}

// Create adds a stack to an org, with the production environment every stack
// starts with. The slug is derived, and it is the slug that has to be unique:
// "My App" and "my-app" are the same stack as far as every URL is concerned.
func (s *StackService) Create(ctx context.Context, orgID string, in CreateStack) (*repo.Stack, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("name", "required")
	}
	slug := repo.Slugify(name)
	if slug == "" {
		return nil, invalid("name", "needs at least one letter or number")
	}
	// Checked rather than left to the UNIQUE index: the index answers with a
	// driver error that reaches the user as a 500, and the caller cannot tell
	// it apart from a disk failure.
	if other, _ := s.store.GetStackBySlug(ctx, orgID, slug); other != nil {
		return nil, svcerr.Conflictf("a stack named %q already exists in this organization", other.Name)
	}
	st := &repo.Stack{
		ID: uuid.New().String(), OrgID: orgID, Name: name, Slug: slug,
		Description: in.Description, Settings: "{}", OrgDeclared: in.OrgDeclared,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateStack(ctx, st); err != nil {
		return nil, err
	}
	// Adopt, not Create: the stack cannot be config-managed one statement
	// after it was created, so the structural gate has nothing to say, and
	// three copies of this row (two handlers and orgconf) each built the
	// environment by hand with no rules at all.
	if _, err := s.envs.Adopt(ctx, st, CreateEnv{Name: "Production"}); err != nil {
		return nil, err
	}
	return st, nil
}

// StackPatch is a partial edit of the row. A nil field is untouched.
type StackPatch struct {
	Name        *string
	Description *string
}

// Update renames a stack and edits its description. The name is structural —
// the slug moves with it, and with the slug every URL, CLI path, container
// name and overlay network under the stack — so a config-managed stack
// refuses it: its file owns the name.
func (s *StackService) Update(ctx context.Context, st *repo.Stack, in StackPatch, by Actor) error {
	if st == nil {
		return svcerr.ErrNotFound
	}
	if in.Name != nil {
		if _, err := s.gate.Gate(ctx, st.ID, GateStructural, by.Surface()); err != nil {
			return err
		}
		name := strings.TrimSpace(*in.Name)
		slug := repo.Slugify(name)
		if slug == "" {
			return invalid("name", "needs at least one letter or number")
		}
		if other, _ := s.store.GetStackBySlug(ctx, st.OrgID, slug); other != nil && other.ID != st.ID {
			return svcerr.Conflictf("a stack named %q already exists in this organization", other.Name)
		}
		if slug != st.Slug {
			// Writes the row itself, stops every tile and redeploys what was
			// running. Inline is fine: the redeploy only enqueues.
			if err := s.renamer.RenameStackNow(ctx, st, name); err != nil {
				return err
			}
		} else {
			// Same slug, so nothing under the stack moves: a display-name
			// edit only.
			st.Name = name
		}
	}
	if in.Description != nil {
		st.Description = *in.Description
	}
	return s.store.UpdateStack(ctx, st)
}

// Reslug changes only the slug, leaving the display name alone. It is the
// `moved:` directive's half of a rename: an org config file's `moved:` block
// declares that a stack changed slug, and the stack's own apply — which runs
// immediately after, and is the only thing that knows the file's name for it
// — does the rest.
//
// It exists because that path used to write Name and Slug both, from the
// slug, so a stack called "Billing API" that moved to `billing` came out
// displayed as "billing" until someone renamed it back by hand.
func (s *StackService) Reslug(ctx context.Context, st *repo.Stack, slug string) error {
	if st == nil {
		return svcerr.ErrNotFound
	}
	st.Slug = slug
	return s.store.UpdateStack(ctx, st)
}

// Delete tears the stack down and removes it.
//
// The rows go on cascade, but the services have to be stopped and the pooled
// overlays handed back by name first: a freed network that still has services
// on it is handed to the next claim, possibly another org's. The error is
// returned rather than swallowed; delete again is the retry.
func (s *StackService) Delete(ctx context.Context, st *repo.Stack) error {
	if st == nil {
		return svcerr.ErrNotFound
	}
	if err := s.ops.TeardownStack(ctx, st); err != nil {
		return err
	}
	if err := s.store.DeleteStack(ctx, st.ID); err != nil {
		return err
	}
	// The cascade took every tile's cron_jobs and backups rows with it, and a
	// scheduler still holding them fires at tiles that are gone. The panel
	// never did this; only the API did.
	s.sched.Reload(ctx)
	return nil
}

// Move puts a stack in another org. Both orgs' write rights are the caller's
// to check — this only refuses the slug collision, which would otherwise be
// a UNIQUE error after the point of no return.
func (s *StackService) Move(ctx context.Context, st *repo.Stack, toOrgID string) error {
	if st == nil {
		return svcerr.ErrNotFound
	}
	if other, _ := s.store.GetStackBySlug(ctx, toOrgID, st.Slug); other != nil {
		return svcerr.Conflictf("that organization already has a stack with the slug %q", st.Slug)
	}
	from := st.OrgID
	if err := s.store.SetStackOrg(ctx, st.ID, toOrgID); err != nil {
		return err
	}
	st.OrgID = toOrgID
	// Everything minted under the old org's rights stops here — see
	// RevokeService. A move is the one change where the resource walks out
	// from under the people holding links to it.
	return s.revoke.StackMoved(ctx, st.ID, from)
}

// BindConfig is a stack's link to the repository that owns it.
type BindConfig struct {
	ConnectorID string
	Repo        string // "owner/name", or a github URL this normalises
	Branch      string
	Path        string
	// OrgDeclared marks the binding as an org config file's doing. Only
	// orgconf sets it, and only to true: a person editing the binding of a
	// stack an org file declared does not un-declare it, because the file
	// still declares it and the next org apply would put the binding back
	// anyway. Clearing the flag is what Unbind is for.
	OrgDeclared bool
}

// Bind points a stack at a config repository, making the file the owner of
// its structure from here on. Planning is the caller's next move, not this
// method's: the panel needs the plans back to report them, and an org config
// apply plans the stack a moment later on its own schedule.
//
// A config apply wrote this binding with none of the three things the panel
// does: it took a connector without checking it belonged to the stack's org
// (a connector is a credential), it stored whatever repo string it was given
// rather than the normalised owner/name, and it left the staged rows in
// place. Those rows were recorded while the stack was UI-managed; the file
// owns the stack now, so applying them would be a structural write the lock
// exists to block.
func (s *StackService) Bind(ctx context.Context, st *repo.Stack, in BindConfig) error {
	if st == nil {
		return svcerr.ErrNotFound
	}
	if in.ConnectorID == "" {
		return invalid("connector_id", "required")
	}
	// A connector grants credentials, so only one from the stack's own org.
	cn, err := s.store.GetConnector(ctx, in.ConnectorID)
	if err != nil || cn == nil || cn.OrgID != st.OrgID {
		return invalid("connector_id", "unknown connector")
	}
	repoFull := strings.TrimSpace(in.Repo)
	if repoFull == "" {
		return invalid("repo", "repository required (owner/name)")
	}
	st.ConfigConnectorID = in.ConnectorID
	st.ConfigRepo = strings.TrimSuffix(strings.TrimPrefix(repoFull, "https://github.com/"), ".git")
	st.ConfigBranch = strings.TrimSpace(in.Branch)
	st.ConfigPath = strings.TrimSpace(in.Path)
	if in.OrgDeclared {
		st.OrgDeclared = true
	}
	if err := s.store.UpdateStack(ctx, st); err != nil {
		return err
	}
	s.dropStaged(ctx, st)
	return nil
}

// Unbind hands the stack back to the UI. The staged rows are not dropped
// here: with the file gone there is nothing left to block them, and they are
// the user's own unapplied edits.
func (s *StackService) Unbind(ctx context.Context, st *repo.Stack) error {
	if st == nil {
		return svcerr.ErrNotFound
	}
	st.ConfigConnectorID, st.ConfigRepo, st.ConfigBranch, st.ConfigPath = "", "", "", ""
	st.OrgDeclared = false
	return s.store.UpdateStack(ctx, st)
}

// dropStaged clears every environment's pending set. Best effort: a binding
// that half-succeeded is worse than a staged row that outlives its window,
// and the gate refuses to apply them anyway once the stack is managed.
func (s *StackService) dropStaged(ctx context.Context, st *repo.Stack) {
	envs, err := s.store.ListEnvironmentsByStack(ctx, st.ID)
	if err != nil {
		slog.Error("staged rows not dropped on config lock", "stack", st.ID, "error", err)
		return
	}
	for _, e := range envs {
		if err := s.store.DeleteStagedByEnv(ctx, e.ID); err != nil {
			slog.Error("staged rows not dropped on config lock", "stack", st.ID, "env", e.ID, "error", err)
		}
	}
}

// --- reads ---

// Get is one stack by id.
func (s *StackService) Get(ctx context.Context, id string) (*repo.Stack, error) {
	st, err := s.store.GetStack(ctx, id)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, svcerr.ErrNotFound
	}
	return st, nil
}

// BySlug is one stack by its slug within an org.
func (s *StackService) BySlug(ctx context.Context, orgID, slug string) (*repo.Stack, error) {
	st, err := s.store.GetStackBySlug(ctx, orgID, slug)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, svcerr.ErrNotFound
	}
	return st, nil
}

// ListForOrg is an org's stacks.
func (s *StackService) ListForOrg(ctx context.Context, orgID string) ([]repo.Stack, error) {
	return s.store.ListStacksByOrg(ctx, orgID)
}

// ListAll is every stack on the server, for the admin area and the jobs that
// sweep all of them. Not org-scoped; see the note on TileService.ListAll.
func (s *StackService) ListAll(ctx context.Context) ([]repo.Stack, error) {
	return s.store.ListStacks(ctx)
}
