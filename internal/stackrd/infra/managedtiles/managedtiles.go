// Package databases provisions and manages database containers: per-engine
// image/env/volume defaults plus dump & restore commands.
package managedtiles

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/netpool"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Engine describes one supported database engine.
type Engine struct {
	DefaultImage string
	Port         int
	DataPath     string
	// HTTP marks an engine that speaks HTTP on Port, so Traefik can put a
	// domain in front of it. Wire-protocol engines (postgres, mysql, redis…)
	// cannot be proxied by an HTTP router, a domain on one would resolve and
	// then never work, which is worse than not offering it.
	HTTP bool
	// Env builds container env for first boot.
	Env func(d *repo.Tile) []string
	// Cmd overrides the container command (nil = image default).
	Cmd func(d *repo.Tile) []string
	// DumpCmd streams a logical dump to stdout inside the container.
	//
	// DumpCmd/RestoreCmd have had no caller since the backups
	// feature was removed. Kept deliberately, the backup rethink and db
	// forking both need exactly this table, and re-deriving the per-engine
	// flags is the expensive part.
	DumpCmd func(d *repo.Tile) []string
	// RestoreCmd consumes a dump from stdin inside the container (nil = restore unsupported).
	RestoreCmd func(d *repo.Tile) []string

	// RootUser overrides the generated admin user ("" = the tile slug).
	RootUser string
	// SecretSuffix ends the provision's (vestigial) secret name.
	SecretSuffix string
	// PrimaryOutput is the output a consumer reads when it names none.
	PrimaryOutput string
	// AutoInjectAll marks engines whose slices need their whole output set
	// wired into a consumer (endpoint + bucket + keys), not one url.
	AutoInjectAll bool
	// PublicSlices marks engines whose slices can be published read-only
	// (s3 bucket policies); everything else rejects the request.
	PublicSlices bool

	// Conn are the connection details published on the instance tile itself,
	// for a consumer that just uses the database rather than being cut a
	// slice of it. The first entry is the primary one, what the drawer's
	// Copy button copies. nil = this engine publishes nothing.
	Conn func(d *repo.Tile) []ConnVar

	// SliceName normalises a requested slice name into what the engine can
	// actually create (SQL engines fold "-" to "_"). nil = the name is taken
	// as-is. Every reader of a slice name goes through the SliceName func
	// below so the config diff compares what the provisioner stored.
	SliceName func(name string) string

	// Provisioning hooks. A nil Provision means the engine cannot cut
	// per-consumer slices; everything else is optional and degrades to
	// "nothing to do" rather than to a special case somewhere else.
	Provision func(s *Service, ctx context.Context, instance, consumer *repo.Tile, name string, public bool) (*repo.Provision, error)
	Drop      func(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision) error
	// Fork copies a live slice's data into dst, which ForkSlice has already
	// provisioned empty on the same instance. nil = this engine can provision
	// but not copy, which is a clean error rather than a silent empty fork.
	Fork       func(s *Service, ctx context.Context, instance *repo.Tile, src, dst *repo.Provision) error
	Ensure     func(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision, w io.Writer)
	Outputs    func(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision) []repo.ResourceOutput
	Ready      func(s *Service, ctx context.Context, instance *repo.Tile, containerID string) error
	SliceStats func(s *Service, ctx context.Context, instance *repo.Tile, names []string) (map[string]SliceStat, error)

	// Display. SliceNoun/UnitNoun name the engine's parts in the UI
	// ("logical db"/"database" vs "bucket"/"bucket"); Icon is the sprite
	// symbol for engines with a real logo ("" = draw the stroke glyph named
	// by Shape, default a cylinder); SlicePath is the URL segment of the
	// engine's own per-slice UI ("" = slices open the instance drawer).
	SliceNoun string
	UnitNoun  string
	Icon      string
	Shape     string
	SlicePath string
	// Label is the engine's product name for pickers; Order sorts them there
	// (lowest first) so the list has an opinion without a second list to keep.
	Label string
	Order int
	// DataBrowser/FileBrowser mark the engine-owned content UIs, so the
	// handlers and the drawer tabs ask the registry instead of the engine
	// name.
	DataBrowser bool
	FileBrowser bool
}

// ConnVar is one connection detail published on an instance tile.
type ConnVar struct {
	Name   string
	Value  string
	Secret bool
}

// SliceName is the name engine would give a slice asked for as name, before
// uniquifying. The one place a config name and a stored name meet.
func SliceName(engine, name string) string {
	if e, ok := Engines[engine]; ok && e.SliceName != nil {
		return e.SliceName(name)
	}
	return name
}

var Engines = map[string]Engine{
	"postgres": {
		SliceName:     sqlIdent,
		Label:         "PostgreSQL",
		Order:         1,
		DefaultImage:  "postgres:17",
		Port:          5432,
		DataPath:      "/var/lib/postgresql/data",
		SecretSuffix:  "_url",
		PrimaryOutput: "DATABASE_URL",
		SliceNoun:     "logical db",
		UnitNoun:      "database",
		Icon:          "",
		SlicePath:     "pgdbs",
		DataBrowser:   true,
		Env: func(d *repo.Tile) []string {
			return []string{"POSTGRES_DB=" + d.DBName, "POSTGRES_USER=" + d.DBUser, "POSTGRES_PASSWORD=" + d.DBPassword}
		},
		DumpCmd: func(d *repo.Tile) []string {
			return []string{"env", "PGPASSWORD=" + d.DBPassword, "pg_dump", "-U", d.DBUser, "-d", d.DBName}
		},
		// Wipe, then restore. A bare `psql < dump` merges: pg_dump emits no
		// DROP statements, so CREATE TABLE fails on a table that is already
		// there and the COPY behind it lands on top, which duplicates every
		// row. Measured on the rig: restoring a one-row dump over a two-row
		// table gave three rows and reported success, while the confirm
		// dialog promises the current data is overwritten.
		//
		// ON_ERROR_STOP so a restore that dies halfway is reported instead of
		// leaving a half-restored database looking fine.
		//
		// only the public schema is dropped. A dump that creates
		// its own schemas restores into whatever of them is already there.
		// Recreating the database itself is the complete answer and needs the
		// server to have no connection open to it, which is a bigger change.
		//
		// Second ceiling from the same line: DROP SCHEMA takes locks, so a
		// restore now waits on live connections to the instance's own
		// database where before it merged into them. Slices are separate
		// databases and do not hold it, so the usual consumer has no effect.
		RestoreCmd: func(d *repo.Tile) []string {
			script := `PGPASSWORD="$0" psql -v ON_ERROR_STOP=1 -U "$1" -d "$2" ` +
				`-c 'DROP SCHEMA public CASCADE' -c 'CREATE SCHEMA public' >/dev/null && ` +
				`PGPASSWORD="$0" psql -v ON_ERROR_STOP=1 -U "$1" -d "$2" >/dev/null`
			// sh, not bash: the alpine postgres images have no bash.
			return []string{"sh", "-c", script, d.DBPassword, d.DBUser, d.DBName}
		},
	},
	"s3": {
		Label:         "S3 object storage",
		Order:         5,
		DefaultImage:  "rustfs/rustfs:latest",
		Port:          9000,
		HTTP:          true,
		DataPath:      "/data",
		PrimaryOutput: "S3_ENDPOINT",
		AutoInjectAll: true,
		PublicSlices:  true,
		SliceNoun:     "bucket",
		UnitNoun:      "bucket",
		Shape:         "bucket",
		SlicePath:     "buckets",
		FileBrowser:   true,
		Env: func(d *repo.Tile) []string {
			return []string{"RUSTFS_ACCESS_KEY=" + d.DBUser, "RUSTFS_SECRET_KEY=" + d.DBPassword}
		},
		Cmd: func(d *repo.Tile) []string { return []string{"--console-enable", "/data"} },
	},
}

// Behaviour is wired here rather than in the map literal above: these hooks
// read Engines themselves (port lookups), and a map initializer that named
// them would be an initialization cycle. Adding an engine means adding its
// entry above and, if it provisions, one line here.
func init() {
	wire("postgres", Engine{
		Conn: pgConn, Provision: pgProvision, Drop: pgDrop, Fork: pgFork, Ensure: pgEnsure,
		Outputs: pgOutputs, Ready: pgReady, SliceStats: pgSliceStats,
	})
	wire("s3", Engine{
		Conn: s3Conn, Provision: s3Provision, Drop: s3Drop, Fork: s3Fork,
		Ensure: s3Ensure, Outputs: s3Outputs, Ready: s3Ready,
	})
}

// wire copies the non-nil hooks of src onto the registered engine.
func wire(name string, src Engine) {
	e := Engines[name]
	e.Conn, e.Provision, e.Drop, e.Fork = src.Conn, src.Provision, src.Drop, src.Fork
	e.Ensure, e.Outputs, e.Ready, e.SliceStats = src.Ensure, src.Outputs, src.Ready, src.SliceStats
	Engines[name] = e
}

// connHost is the hostname a consumer dials. The tile alias, not the slug:
// both resolve (Deploy joins the container under each), but only the alias
// survives a rename, the slug alias is whatever the container was started
// with, so renaming a tile silently breaks every consumer that stored the old
// one. Provisioned slices already publish the alias (pgVars); this is the
// same fact, so it has to be the same answer.
func connHost(d *repo.Tile) string { return envnet.TileAlias(d.ID) }

func hostPort(d *repo.Tile) string {
	return fmt.Sprintf("%s:%d", connHost(d), Engines[d.Engine].Port)
}

// EngineOption is one entry of the engine picker.
type EngineOption struct {
	Name  string // registry key, the value submitted
	Label string // product name
}

// EngineOptions lists the engines a user can create, in Order. Registering an
// engine is what puts it in the picker, there is no second list.
func EngineOptions() []EngineOption {
	out := make([]EngineOption, 0, len(Engines))
	for name, e := range Engines {
		label := e.Label
		if label == "" {
			label = name
		}
		out = append(out, EngineOption{Name: name, Label: label})
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := Engines[out[i].Name].Order, Engines[out[j].Name].Order
		if oi != oj {
			return oi < oj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SpeaksHTTP reports whether this engine can sit behind Traefik.
func SpeaksHTTP(engine string) bool { return Engines[engine].HTTP }

// Service manages database container lifecycle.
//
// Every docker call goes through the cluster, which decides socket or agent.
// This package used to carry its own copy of that decision behind an
// optional WithNodes, and twelve of the thirteen places that built a Service
// never called it, so every exec ran against the manager whatever node the
// instance was on (docs/plans/35-cluster.md).
type Service struct {
	c     *cluster.Cluster
	store repo.Store
}

func NewService(c *cluster.Cluster, store repo.Store) *Service {
	return &Service{c: c, store: store}
}

// locate is where an instance's container is right now, id and node, from
// swarm task state. The local socket only answers for the manager, so an
// instance on a worker used to read as "not running" rather than "not here".
func (s *Service) locate(ctx context.Context, d *repo.Tile) (cid, node string, err error) {
	tasks, err := s.c.RunningTasks(ctx, s.ServiceName(ctx, d))
	if err != nil || len(tasks) == 0 {
		return "", "", err
	}
	return tasks[0].ContainerID, tasks[0].NodeID, nil
}

// execOn runs cmd in the instance's container on whichever node holds it and
// returns the combined output, the shape every engine helper wants.
//
// looks the task up again even though the caller just did, to get
// the node. One manager API call per exec; provisioning makes a handful.
// Thread the node through the engine helpers if that ever shows up.
func (s *Service) execOn(ctx context.Context, d *repo.Tile, cid string, cmd []string) (string, error) {
	_, node, err := s.locate(ctx, d)
	if err != nil {
		return "", err
	}
	if node == "" {
		return "", fmt.Errorf("instance %s is not running", d.Slug)
	}
	return s.c.Exec(ctx, node, cid, cmd)
}

// NewDB fills engine defaults and generated credentials into a DB record.
func NewDB(d *repo.Tile) error {
	eng, ok := Engines[d.Engine]
	if !ok {
		return fmt.Errorf("unknown engine %q", d.Engine)
	}
	// A pre-set image is a per-instance override (version pin, immich's
	// wire-compatible pg build), only fill the engine default when unset.
	if d.ImageRef == "" {
		d.ImageRef = eng.DefaultImage
	}
	// Slug, not display name: db identifiers must not contain spaces.
	d.DBName = d.Slug
	d.DBUser = d.Slug
	if eng.RootUser != "" {
		d.DBUser = eng.RootUser
	}
	d.DBPassword = secrets.RandomHex(16)
	return nil
}

// VolumeName is the named Docker volume holding this DB's data. Keeps the
// "stackr-" prefix, see repo.StorageVolume for why.
func VolumeName(d *repo.Tile) string {
	return "stackr-db-" + d.ID[:8]
}

// DataVolume describes the docker volume holding a DB's data: what the graph's
// tucked volume card opens. MountPath is where the engine sees it inside the
// container; the rest comes from docker.
type DataVolume struct {
	runtime.VolumeInfo
	MountPath string
}

// DataVolume inspects a DB tile's data volume. A volume that docker doesn't
// know about yet (tile created, never deployed) still returns its name and
// mount path with an unknown size.
func (s *Service) DataVolume(ctx context.Context, d *repo.Tile) DataVolume {
	out := DataVolume{
		VolumeInfo: runtime.VolumeInfo{Name: VolumeName(d), SizeBytes: -1},
		MountPath:  Engines[d.Engine].DataPath,
	}
	if info, err := s.c.InspectVolume(ctx, out.Name); err == nil {
		out.VolumeInfo = info
	}
	return out
}

// Conn lists the connection details published on a DB tile itself.
func Conn(d *repo.Tile) []ConnVar {
	eng, ok := Engines[d.Engine]
	if !ok || eng.Conn == nil {
		return nil
	}
	return eng.Conn(d)
}

// URL is the primary connection detail for a DB tile, the first thing Conn
// publishes.
//
// no caller since the drawer started listing Conn in full. Kept
// because "the one string that identifies this instance" is what a CLI
// `stackr db url` and the config-as-code exporter both want, and rederiving
// "first entry" at each of them is worse.
func URL(d *repo.Tile) string {
	if cs := Conn(d); len(cs) > 0 {
		return cs[0].Value
	}
	return ""
}

// PublishConnection stores/refreshes a db tile's own connection details as
// variables on that tile, so a consumer can read them directly with
// ${{ tile.<slug>.DATABASE_URL }}, the case where a service just uses the
// database rather than being provisioned a slice of it.
//
// They live as tile variables rather than a managed resource because a
// resource sharing the tile's slug would make every reference ambiguous.
func PublishConnection(ctx context.Context, store repo.Store, d *repo.Tile) {
	now := time.Now().UTC()
	for _, c := range Conn(d) {
		_ = store.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: d.ID,
			Name: c.Name, Value: c.Value, Secret: c.Secret, CreatedAt: now, UpdatedAt: now})
	}
}

// declaredEnv resolves the db tile's own env, only the variables it declares,
// not the connection details PublishConnection stores on it for consumers.
// Those describe how to reach the database; feeding them back into its own
// container would override the engine's settings with a client's view of them.
func (s *Service) declaredEnv(ctx context.Context, d *repo.Tile) ([]string, error) {
	res, err := varref.New(s.store).Resolve(ctx, d.ID, varref.System)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range envutil.Parse(d.Env) {
		if val, ok := res.Vars[v.Key]; ok {
			out = append(out, v.Key+"="+val)
		}
	}
	return out, nil
}

// Deploy pulls the image and creates or updates the DB's swarm service.
//
// A managed db is a pinned tile: one replica, stop-first, placed on the node
// that holds its volume. Two tasks on one data directory corrupt it, and a
// task scheduled onto a node with an empty disk is silent data loss, not an
// error (docs/plans/30-docker-swarm.md, the volume addendum).
//
// Every network the instance needs is in the spec before the service exists.
// A service cannot join one afterwards without rolling every task, so the
// shared network a provisioned instance's consumers reach it on is claimed
// here, not connected after the fact.
func (s *Service) Deploy(ctx context.Context, d *repo.Tile) error {
	eng, ok := Engines[d.Engine]
	if !ok {
		return fmt.Errorf("unknown engine %q", d.Engine)
	}
	sc, netName, err := envnet.Ensure(ctx, s.store, s.c, d)
	if err != nil {
		return fmt.Errorf("environment network: %w", err)
	}
	if err := s.c.PullImage(ctx, d.ImageRef, io.Discard); err != nil {
		return fmt.Errorf("pull %s: %w", d.ImageRef, err)
	}

	// Self-heal: republish the tile's connection details so a consumer that
	// references them resolves current values even after a password rotation.
	PublishConnection(ctx, s.store, d)
	env := eng.Env(d)
	extra, err := s.declaredEnv(ctx, d)
	if err != nil {
		return err // never run with unresolved references
	}
	env = append(env, extra...)
	var cmd []string
	if eng.Cmd != nil {
		cmd = eng.Cmd(d)
	}
	ports := map[string]string{}
	if d.ExternalPort > 0 {
		ports[strconv.Itoa(d.ExternalPort)] = strconv.Itoa(eng.Port)
	}
	nets := []runtime.NetAttach{{Name: netName, Aliases: []string{d.Slug, envnet.TileAlias(d.ID)}}}
	if ps, _ := s.store.ListProvisionsByInstance(ctx, d.ID); len(ps) > 0 {
		if err := s.ensureSharedNet(ctx, d); err != nil {
			return err
		}
		nets = append(nets, runtime.NetAttach{Name: SharedNet(d), Aliases: instanceAliases(d)})
	}
	cpuLimit, memLimit := settings.ForTile(ctx, s.store, d).EffectiveLimits(d.CPULimit, d.MemLimitMB)
	// A managed database always holds a volume, so it is always pinned and
	// always needs a home node before swarm is allowed to place it.
	place, err := placement.For(ctx, s.store, s.c.Runtime(), d)
	if err != nil {
		return err
	}
	since := time.Now()
	created, err := s.c.EnsureService(ctx, runtime.ServiceSpec{
		Name:       sc.ServiceName(d.Slug),
		Image:      d.ImageRef,
		Cmd:        cmd,
		Env:        env,
		Labels:     map[string]string{runtime.LabelDB: d.ID},
		Mounts:     []string{VolumeName(d) + ":" + eng.DataPath},
		Ports:      ports,
		Networks:   nets,
		CPULimit:   cpuLimit,
		MemLimitMB: memLimit,
		ShmSizeMB:  d.ShmSizeMB,
		Pinned:     true,
		HomeNode:   place.HomeNode,
		NodeGroup:  place.Group,
		StopGraceS: dbStopGraceS,
	})
	if err != nil {
		return err
	}
	// Callers reach straight for ContainerID and WaitReady after this returns;
	// a service create is asynchronous, so wait for the task to exist first.
	// An update waits on this roll, not the previous one swarm still reports
	// as completed (WaitRolled).
	if created {
		since = time.Time{}
	}
	if err := s.c.WaitRolled(ctx, sc.ServiceName(d.Slug), since, 90*time.Second); err != nil {
		return err
	}
	s.selfHealProvisions(d)
	// Managed deploys bypass the engine's OnFinish, so baseline the image
	// digest here or the registry watcher never learns what runs.
	if dg, err := s.c.LocalDigest(ctx, d.ImageRef); err == nil && dg != "" {
		_ = s.store.SetTileImageDigest(ctx, d.ID, dg)
	}
	return nil
}

// dbStopGraceS gives an engine time to checkpoint and shut down cleanly.
// Swarm's default is 10s, which SIGKILLs postgres mid-checkpoint.
const dbStopGraceS = 60

// selfHealProvisions kicks the instance-side provision reconcile in the
// background: waits for the store to come ready, then recreates any slice a
// consumer provisioned while this instance was down. Detached, a deploy/start
// must not block on readiness polling.
func (s *Service) selfHealProvisions(d *repo.Tile) {
	tile := *d
	go s.ReconcileOwnProvisions(context.Background(), &tile, slogWriter{instance: d.Slug})
}

// ContainerID returns the current container ID for the DB ("" if none).
// The service's task container carries the same label the plain container did,
// so exec, stats and the sql browser keep their call.
func (s *Service) ContainerID(ctx context.Context, d *repo.Tile) (string, error) {
	cid, _, err := s.locate(ctx, d)
	return cid, err
}

// ServiceName is the swarm service the instance runs as ("" when its scope
// cannot be resolved).
func (s *Service) ServiceName(ctx context.Context, d *repo.Tile) string {
	return envnet.ServiceFor(ctx, s.store, d)
}

// Stop scales the service to zero, keeping the service and its data volume.
// Stopping the task container instead would just get a replacement.
func (s *Service) Stop(ctx context.Context, d *repo.Tile) error {
	name := s.ServiceName(ctx, d)
	if name == "" {
		return nil
	}
	return s.c.ScaleService(ctx, name, 0)
}

// Start scales a stopped instance back to one replica, or deploys it when it
// has no service yet.
func (s *Service) Start(ctx context.Context, d *repo.Tile) error {
	name := s.ServiceName(ctx, d)
	if name == "" {
		return s.Deploy(ctx, d)
	}
	if _, err := s.c.ServiceStatus(ctx, name); err != nil {
		return s.Deploy(ctx, d)
	}
	since := time.Now()
	if err := s.c.ScaleService(ctx, name, 1); err != nil {
		return err
	}
	// Shorter than a deploy's: nothing is being pulled, the image is on disk.
	if err := s.c.WaitRolled(ctx, name, since, 30*time.Second); err != nil {
		return err
	}
	s.selfHealProvisions(d)
	return nil
}

// Remove removes the DB service (data volume is left behind on purpose).
// Its shared network goes back to the pool, callers gate delete on there
// being no remaining provisions, so nothing else is attached.
//
// Service first, network second. Releasing first put the name back in the pool
// while the instance's own service was still attached to it, so the next
// environment to claim that name inherited a database it could reach by DNS.
func (s *Service) Remove(ctx context.Context, d *repo.Tile) error {
	if name := s.ServiceName(ctx, d); name != "" {
		if err := s.c.RemoveService(ctx, name); err != nil {
			return err
		}
	}
	return netpool.ReleaseDB(ctx, s.store, s.c.Runtime(), d)
}
