package store

import (
	"context"
	"time"
)

// Tile is a row of tiles. JSON-shaped columns stay strings: the tile leaf
// parses them.
type Tile struct {
	ID                      string    `db:"id"`
	StackID                 string    `db:"stack_id"`
	EnvironmentID           string    `db:"environment_id"`
	Name                    string    `db:"name"`
	Slug                    string    `db:"slug"`
	Kind                    string    `db:"kind"`
	GitURL                  string    `db:"git_url"`
	GitBranch               string    `db:"git_branch"`
	ImageRef                string    `db:"image_ref"`
	DockerfilePath          string    `db:"dockerfile_path"`
	BuildContext            string    `db:"build_context"`
	WatchPaths              string    `db:"watch_paths"`
	EnvJSON                 string    `db:"env_json"`
	BuildArgs               string    `db:"build_args"`
	Volumes                 string    `db:"volumes"`
	Command                 string    `db:"command"`
	ContainerPort           int       `db:"container_port"`
	PublishedPorts          string    `db:"published_ports"`
	EndpointProtocol        string    `db:"endpoint_protocol"`
	HealthPath              string    `db:"health_path"`
	HealthcheckCmd          string    `db:"healthcheck_cmd"`
	HealthcheckIntervalS    int       `db:"healthcheck_interval_s"`
	HealthcheckTimeoutS     int       `db:"healthcheck_timeout_s"`
	HealthcheckRetries      int       `db:"healthcheck_retries"`
	HealthcheckStartPeriodS int       `db:"healthcheck_start_period_s"`
	CPULimit                float64   `db:"cpu_limit"`
	MemLimitMB              int       `db:"mem_limit_mb"`
	User                    string    `db:"user"`
	ShmSizeMB               int       `db:"shm_size_mb"`
	Privileged              bool      `db:"privileged"`
	Devices                 string    `db:"devices"`
	RestartPolicy           string    `db:"restart_policy"`
	DependsOn               string    `db:"depends_on"`
	Files                   string    `db:"files"`
	SharedNet               string    `db:"shared_net"`
	Replicas                int       `db:"replicas"`
	UpdatePolicy            string    `db:"update_policy"`
	TagPolicy               string    `db:"tag_policy"`
	CreatedAt               time.Time `db:"created_at"`
	UpdatedAt               time.Time `db:"updated_at"`
}

type TileStore interface {
	Create(ctx context.Context, t Tile) error
	Get(ctx context.Context, id string) (Tile, error)
	GetBySlug(ctx context.Context, envID, slug string) (Tile, error)
	ListByEnv(ctx context.Context, envID string) ([]Tile, error)
	ListByStack(ctx context.Context, stackID string) ([]Tile, error)
	Update(ctx context.Context, t Tile) error
	Delete(ctx context.Context, id string) error
}

var tilesT = newTable[Tile]("tiles", nil)

type tiles struct{ crud[Tile] }

func (s tiles) GetBySlug(ctx context.Context, envID, slug string) (Tile, error) {
	return s.one(ctx, "environment_id = ? AND slug = ?", envID, slug)
}

func (s tiles) ListByEnv(ctx context.Context, envID string) ([]Tile, error) {
	return s.many(ctx, "environment_id = ?", envID)
}

func (s tiles) ListByStack(ctx context.Context, stackID string) ([]Tile, error) {
	return s.many(ctx, "stack_id = ?", stackID)
}

// Image is a row of images: a built image and the watch cache, by ref.
type Image struct {
	ID         string     `db:"id"`
	Ref        string     `db:"ref"`
	Digest     string     `db:"digest"`
	BuiltAt    *time.Time `db:"built_at"`
	LastDigest string     `db:"last_digest"`
	LastTag    string     `db:"last_tag"`
	LastError  string     `db:"last_error"`
	CheckedAt  *time.Time `db:"checked_at"`
	CreatedAt  time.Time  `db:"created_at"`
}

type ImageStore interface {
	Create(ctx context.Context, i Image) error
	Get(ctx context.Context, id string) (Image, error)
	GetByRef(ctx context.Context, ref string) (Image, error)
	List(ctx context.Context) ([]Image, error)
	Update(ctx context.Context, i Image) error
	Delete(ctx context.Context, id string) error
}

var imagesT = newTable[Image]("images", nil)

type images struct{ crud[Image] }

func (s images) GetByRef(ctx context.Context, ref string) (Image, error) {
	return s.one(ctx, "ref = ?", ref)
}

func (s images) List(ctx context.Context) ([]Image, error) { return s.many(ctx, "1 = 1") }

// Domain is a row of domains.
type Domain struct {
	ID            string    `db:"id"`
	TileID        string    `db:"tile_id"`
	Host          string    `db:"host"`
	Path          string    `db:"path"`
	ContainerPort int       `db:"container_port"`
	HTTPS         bool      `db:"https"`
	ForceHTTPS    bool      `db:"force_https"`
	RedirectTo    string    `db:"redirect_to"`
	Auto          bool      `db:"auto"`
	Position      int       `db:"position"`
	ProxyJSON     string    `db:"proxy_json"`
	RawCaddy      string    `db:"raw_caddy"`
	CreatedAt     time.Time `db:"created_at"`
}

type DomainStore interface {
	Create(ctx context.Context, d Domain) error
	Get(ctx context.Context, id string) (Domain, error)
	ListByTile(ctx context.Context, tileID string) ([]Domain, error)
	List(ctx context.Context) ([]Domain, error)
	Update(ctx context.Context, d Domain) error
	Delete(ctx context.Context, id string) error
}

var domainsT = newTable[Domain]("domains", nil)

type domains struct{ crud[Domain] }

func (s domains) ListByTile(ctx context.Context, tileID string) ([]Domain, error) {
	return s.many(ctx, "tile_id = ?", tileID)
}

func (s domains) List(ctx context.Context) ([]Domain, error) { return s.many(ctx, "1 = 1") }
