package managedtiles

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// EnsureProvisions reconciles a consumer's provisioned dependencies before it
// deploys. Each provisions row is desired state, so a db/role/bucket that was
// dropped out from under the consumer (and its connection secret) is
// recreated from the row. The secret is always republished (it needs no
// instance); the backing store is best-effort, so a down instance never fails
// the consumer's deploy (the app retries its own connection).
//
// runs on every deploy, one psql/S3 round-trip per provisioned dep.
// Deploys are not hot-path; gate it on "secret missing" if that ever bites.
func (s *Service) EnsureProvisions(ctx context.Context, consumer *repo.Tile, w io.Writer) {
	ps, err := s.store.ListProvisionsByConsumer(ctx, consumer.ID)
	if err != nil || len(ps) == 0 {
		return
	}
	// One readiness verdict per instance, several deps may share one.
	readiness := map[string]error{}
	for i := range ps {
		p := &ps[i]
		inst, err := s.store.GetTile(ctx, p.InstanceTileID)
		if err != nil || inst == nil {
			_, _ = fmt.Fprintf(w, "warning: provision %s: instance missing, skipping\n", p.SecretName)
			continue
		}
		rerr, checked := readiness[inst.ID]
		if !checked {
			rerr = s.WaitReady(ctx, inst, 30*time.Second)
			readiness[inst.ID] = rerr
		}
		if rerr != nil {
			// Best-effort by design: a down dependency never fails the
			// consumer's deploy. The instance-side self-heal (its next
			// start/deploy) recreates whatever is missing.
			_, _ = fmt.Fprintf(w, "warning: %v; skipping reconcile (secret still set)\n", rerr)
		} else {
			s.ensureSlice(ctx, inst, p, w)
		}
		warnSync(w, s.SyncResource(ctx, inst, p))
	}
}

// ensureSlice recreates one slice on its (ready) instance. An engine with no
// Ensure hook has nothing to recreate.
func (s *Service) ensureSlice(ctx context.Context, inst *repo.Tile, p *repo.Provision, w io.Writer) {
	if ensure := Engines[inst.Engine].Ensure; ensure != nil {
		ensure(s, ctx, inst, p, w)
	}
}

// ReconcileOwnProvisions recreates every slice provisioned on this instance,
// the instance-side self-heal, run after the instance starts or redeploys. It
// closes the gap EnsureProvisions leaves open: a consumer deployed while the
// instance was down gets its role/db/bucket the moment the instance returns,
// without a consumer redeploy.
func (s *Service) ReconcileOwnProvisions(ctx context.Context, instance *repo.Tile, w io.Writer) {
	ps, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil || len(ps) == 0 {
		return
	}
	if err := s.WaitReady(ctx, instance, 60*time.Second); err != nil {
		_, _ = fmt.Fprintf(w, "warning: %v; provisions not reconciled\n", err)
		return
	}
	ensured := map[string]bool{} // several consumer rows share one slice
	for i := range ps {
		p := &ps[i]
		if !ensured[p.DBName] {
			ensured[p.DBName] = true
			s.ensureSlice(ctx, instance, p, w)
		}
		warnSync(w, s.SyncResource(ctx, instance, p))
	}
}

// warnSync surfaces a failed resource mirror in the deploy log: the container
// is about to resolve references against those outputs, so a silent failure
// would show up as a confusing "publishes no output named" instead.
func warnSync(w io.Writer, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(w, "warning: publishing resource outputs: %v\n", err)
	}
}

// s3Ensure idempotently recreates the bucket (and its public-read policy).
func s3Ensure(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision, w io.Writer) {
	endpoint, err := s.reachInstance(ctx, instance)
	if err != nil {
		_, _ = fmt.Fprintf(w, "warning: instance %s unreachable; skipping bucket reconcile (secret still set)\n", instance.Slug)
		return
	}
	if err := s.createBucket(ctx, endpoint, instance, p.DBName); err != nil {
		_, _ = fmt.Fprintf(w, "note: bucket reconcile for %s: %v\n", p.DBName, err)
		return
	}
	if p.Public {
		if err := s.putPublicReadPolicy(ctx, endpoint, instance, p.DBName); err != nil {
			_, _ = fmt.Fprintf(w, "note: public-read policy for %s: %v\n", p.DBName, err)
		}
	}
}
