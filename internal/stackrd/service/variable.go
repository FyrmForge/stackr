package service

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Masked is what a secret's value reads as in any listing the caller may not
// see plaintext in. It is not a value: a write that carries it back is a
// caller echoing a listing at us, and storing it would replace the secret
// with three dots.
const Masked = "•••"

// varNameRe is the one variable-name rule. The panel has always enforced it;
// the API accepted any non-empty string, so a name with a space or a `$` in
// it could be stored over the API and then never resolve, or resolve into a
// container env line the shell could not read.
var varNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// VarOwner is what a variable hangs off: a tile, an environment, a stack or
// an organization. The four have different lookup rules and different side
// effects, and every caller used to work that out for itself.
type VarOwner struct{ Kind, ID string }

func TileVars(id string) VarOwner  { return VarOwner{repo.OwnerTile, id} }
func EnvVars(id string) VarOwner   { return VarOwner{repo.OwnerEnv, id} }
func StackVars(id string) VarOwner { return VarOwner{repo.OwnerStack, id} }
func OrgVars(id string) VarOwner   { return VarOwner{repo.OwnerOrg, id} }

// VarWrite is one variable as a caller asked for it. Generate means "mint a
// value if there isn't one", which is why Value and Generate are not
// exclusive: a form sends both and the stored state decides.
type VarWrite struct {
	Name     string
	Value    string
	Secret   bool
	Generate bool
	Length   int
}

// Replanner refreshes a config-managed stack's plans after something a plan
// reads has moved. An interface because the planner lives in
// config/stackconf, which imports this package.
//
// Implementations must return immediately: a replan walks the whole config
// tree and is far slower than the request that triggered it.
type Replanner interface {
	Replan(ctx context.Context, s *repo.Stack)
}

// VariableService owns every write to the variables table, on all four owner
// kinds and from all four surfaces.
//
// It exists for one row above all others: variables written over the API
// never replanned and never redeployed, where the panel did both. `stackr
// vars set` on a running tile, or on a config-managed stack, changed a row in
// the database and nothing a user could see — no redeploy, no refreshed plan,
// and the canvas banner still reporting the missing value that had just been
// supplied.
type VariableService struct {
	store repo.Store
	// deploys owns the redeploy-if-running rule. This used to be a private
	// copy of it, beside three more in the panel's own handlers.
	deploys  *DeployService
	plan     Replanner
	notifier *notify.Notifier
}

func NewVariableService(store repo.Store, deploys *DeployService, plan Replanner,
	n *notify.Notifier) *VariableService {
	return &VariableService{store: store, deploys: deploys, plan: plan, notifier: n}
}

// Set writes the named variables and leaves everything else alone. The verb
// behind a panel form and behind `stackr vars set`.
func (s *VariableService) Set(ctx context.Context, owner VarOwner, writes []VarWrite, by Actor) error {
	if err := s.write(ctx, owner, writes, by); err != nil {
		return err
	}
	return s.after(ctx, owner)
}

// Replace writes the named variables and deletes every other one the owner
// holds: PUT semantics, which is what the API's variable routes offer and
// what `stackr vars` sends after a read-modify-write.
func (s *VariableService) Replace(ctx context.Context, owner VarOwner, writes []VarWrite, by Actor) error {
	if err := s.write(ctx, owner, writes, by); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, w := range writes {
		keep[w.Name] = true
	}
	cur, err := s.store.ListVariables(ctx, owner.Kind, owner.ID)
	if err != nil {
		return err
	}
	for _, v := range cur {
		if keep[v.Name] {
			continue
		}
		if err := s.remove(ctx, owner, v.Name, by); err != nil {
			return err
		}
	}
	// The tile's env blob mirrors its non-secret variables, so a replace that
	// dropped one has to drop it there too or the next tile write projects it
	// straight back (the store's projectEnvVars runs inside every UpdateTile).
	if err := s.syncBlob(ctx, owner); err != nil {
		return err
	}
	return s.after(ctx, owner)
}

// Unset removes variables by name.
func (s *VariableService) Unset(ctx context.Context, owner VarOwner, names []string, by Actor) error {
	for _, n := range names {
		if err := s.remove(ctx, owner, n, by); err != nil {
			return err
		}
	}
	if err := s.syncBlob(ctx, owner); err != nil {
		return err
	}
	return s.after(ctx, owner)
}

// Applied is the side-effect half on its own, for a writer that had to do the
// storing itself. The drop-link submit is the one: its burn and its variable
// writes are a single transaction, so it cannot hand the write over — but it
// used to skip everything after it too, releasing no parked deploy and
// refreshing no plan, which its own comment admitted.
func (s *VariableService) Applied(ctx context.Context, owner VarOwner, names []string) error {
	for _, n := range names {
		s.clearWaiting(ctx, owner, n)
	}
	return s.after(ctx, owner)
}

// write validates and upserts, without the side effects. Validation runs over
// the whole batch first: a set that is going to be refused must not half-apply.
func (s *VariableService) write(ctx context.Context, owner VarOwner, writes []VarWrite, by Actor) error {
	for _, w := range writes {
		if !varNameRe.MatchString(w.Name) {
			return svcerr.Invalidf(w.Name, "variable name: letters, digits, _ . - only, and it cannot start with . or -")
		}
		if w.Value == Masked {
			// Only the API used to catch this. A caller that read a listing and
			// sent it back would otherwise overwrite the secret with the mask.
			return svcerr.Invalidf(w.Name, "was sent back masked; read it unmasked first, or omit it to keep the stored value")
		}
	}
	// Read once: a generate request has to know whether a live value is
	// already there, and asking per name would be a query per variable.
	live := map[string]bool{}
	if cur, err := s.store.ListVariables(ctx, owner.Kind, owner.ID); err == nil {
		for _, v := range cur {
			live[v.Name] = v.Value != ""
		}
	}
	now := time.Now().UTC()
	for _, w := range writes {
		value := w.Value
		if w.Generate {
			if live[w.Name] {
				// Decided, not inherited: generating over a live secret is a
				// silent credential rotation that breaks everything already
				// reading it, and the caller asked for a value, not a rotation.
				// The panel used to overwrite; a deliberate "rotate" control
				// can be added if anyone wants one.
				continue
			}
			length := w.Length
			if length <= 0 {
				length = 32
			}
			// The same generator `default: generated` uses in a config file,
			// so a secret minted here and one minted by an apply are the same
			// kind of thing.
			value = secrets.Generate(length, true, false)
		}
		if err := s.store.UpsertVariable(ctx, &repo.Variable{OwnerKind: owner.Kind, OwnerID: owner.ID,
			Name: w.Name, Value: value, Secret: w.Secret, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		audit.Record(ctx, s.store, by.Audit(), audit.Set, owner.Kind, owner.ID, w.Name)
		s.clearWaiting(ctx, owner, w.Name)
	}
	return nil
}

func (s *VariableService) remove(ctx context.Context, owner VarOwner, name string, by Actor) error {
	if err := s.store.DeleteVariable(ctx, owner.Kind, owner.ID, name); err != nil {
		return err
	}
	audit.Record(ctx, s.store, by.Audit(), audit.Delete, owner.Kind, owner.ID, name)
	return nil
}

// clearWaiting releases a deploy parked on this name. Its reason is gone, and
// without this the tile reads "Waiting for X" for ever. Every handler did
// this; the config apply, the boot backfill and the share-link submit did
// not, and each of those admits it in a comment.
func (s *VariableService) clearWaiting(ctx context.Context, owner VarOwner, name string) {
	switch owner.Kind {
	case repo.OwnerEnv:
		deploy.ClearWaitingEnv(ctx, s.store, owner.ID, name)
	case repo.OwnerStack:
		deploy.ClearWaiting(ctx, s.store, name, owner.ID)
	case repo.OwnerOrg:
		deploy.ClearWaitingOrg(ctx, s.store, owner.ID, name)
	}
}

// syncBlob rewrites a tile's env blob from its non-secret variable rows.
//
// Only on the replace/unset paths, and only for a tile: the blob is a tile
// column the config file owns and the canvas stages edits to (app.SaveEnv),
// so the panel's staged blob edit stays where it is. This is the other writer
// of tile.Env, and it is the one that keeps a PUT of the whole variable set
// from leaving the blob describing a variable that no longer exists.
func (s *VariableService) syncBlob(ctx context.Context, owner VarOwner) error {
	if owner.Kind != repo.OwnerTile {
		return nil
	}
	t, err := s.store.GetTile(ctx, owner.ID)
	if err != nil || t == nil {
		return err
	}
	cur, err := s.store.ListVariables(ctx, owner.Kind, owner.ID)
	if err != nil {
		return err
	}
	var lines []string
	for _, v := range cur {
		if !v.Secret {
			lines = append(lines, v.Name+"="+v.Value)
		}
	}
	sort.Strings(lines)
	blob := strings.Join(lines, "\n")
	if blob == t.Env {
		return nil
	}
	t.Env = blob
	return s.store.UpdateTile(ctx, t.ID, t.TileConfig)
}

// after performs what a variable write earns. This is the half the API had
// none of.
func (s *VariableService) after(ctx context.Context, owner VarOwner) error {
	stackID := s.stackOf(ctx, owner)
	if owner.Kind == repo.OwnerTile {
		s.redeploy(ctx, owner.ID)
	}
	if stackID == "" {
		return nil
	}
	if st, err := s.store.GetStack(ctx, stackID); err == nil && st != nil && st.ConfigManaged() && s.plan != nil {
		// A changed value alters plan validity: a missing-value error is what
		// the canvas banner reports, and it should heal without someone
		// pressing "Plan now".
		s.plan.Replan(ctx, st)
	}
	if s.notifier != nil {
		s.notifier.Project(stackID)
	}
	return nil
}

// redeploy restarts a running service whose variables moved. Env is read at
// container start, so without this a just-added reference silently never
// connects until something else redeploys the tile.
//
// All four clauses matter. A cron applies its env on the next scheduled run;
// a volume has no container; and a managed instance's variables are its
// published connection details — enqueueing the build engine for one is the
// bug `POST /apps/{db-id}/deploy` used to be.
func (s *VariableService) redeploy(ctx context.Context, tileID string) {
	if s.deploys == nil {
		return
	}
	s.deploys.RedeployIfRunning(ctx, tileID, "variables")
}

// stackOf is the stack a write's side effects belong to ("" for an org, whose
// variables are not any one stack's).
func (s *VariableService) stackOf(ctx context.Context, owner VarOwner) string {
	switch owner.Kind {
	case repo.OwnerStack:
		return owner.ID
	case repo.OwnerTile:
		if t, err := s.store.GetTile(ctx, owner.ID); err == nil && t != nil {
			return t.StackID
		}
	case repo.OwnerEnv:
		if e, err := s.store.GetEnvironment(ctx, owner.ID); err == nil && e != nil {
			return e.StackID
		}
	}
	return ""
}

// List is an owner's variables, values included.
//
// It does NOT mask. Masking is not a property of the rows, it is a property of
// who is asking — see canReadSecrets in handlers/api/v1/variables.go and
// toVarEntries in the panel — so a service that masked here would either have
// to take the principal or would quietly blind the deploy path, which needs
// the plaintext. The only thing that moves is who may reach the table.
func (s *VariableService) List(ctx context.Context, owner VarOwner) ([]repo.Variable, error) {
	return s.store.ListVariables(ctx, owner.Kind, owner.ID)
}

// Upsert writes one variable row directly, with none of Set's cascade — no
// replan, no redeploy, no blob sync.
//
// Its one caller is the org config wizard, writing values that the apply it is
// about to run will act on anyway. Anything a person edits goes through Set.
func (s *VariableService) Upsert(ctx context.Context, v *repo.Variable) error {
	return s.store.UpsertVariable(ctx, v)
}

// Names is every variable name on the server, values excluded — the search
// palette's index. It carries no owner and no values on purpose: a name is
// not a secret, a value may be.
func (s *VariableService) Names(ctx context.Context) ([]repo.Variable, error) {
	return s.store.ListVariableNames(ctx)
}
