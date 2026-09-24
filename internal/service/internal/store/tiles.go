package store

import (
	"context"
	"time"
)

// Tile is a row of tiles. JSON-shaped columns stay strings: the tile leaf
// parses them.
type Tile struct {
	ID                      string    `db:"id" json:"id"`
	StackID                 string    `db:"stack_id" json:"stack_id"`
	EnvironmentID           string    `db:"environment_id" json:"environment_id"`
	Name                    string    `db:"name" json:"name"`
	Slug                    string    `db:"slug" json:"slug"`
	Kind                    string    `db:"kind" json:"kind"`
	GitURL                  string    `db:"git_url" json:"git_url"`
	GitBranch               string    `db:"git_branch" json:"git_branch"`
	ImageRef                string    `db:"image_ref" json:"image_ref"`
	DockerfilePath          string    `db:"dockerfile_path" json:"dockerfile_path"`
	BuildContext            string    `db:"build_context" json:"build_context"`
	WatchPaths              string    `db:"watch_paths" json:"watch_paths"`
	EnvJSON                 string    `db:"env_json" json:"env_json"`
	BuildArgs               string    `db:"build_args" json:"build_args"`
	Volumes                 string    `db:"volumes" json:"volumes"`
	Command                 string    `db:"command" json:"command"`
	ContainerPort           int       `db:"container_port" json:"container_port"`
	PublishedPorts          string    `db:"published_ports" json:"published_ports"`
	EndpointProtocol        string    `db:"endpoint_protocol" json:"endpoint_protocol"`
	HealthPath              string    `db:"health_path" json:"health_path"`
	HealthcheckCmd          string    `db:"healthcheck_cmd" json:"healthcheck_cmd"`
	HealthcheckIntervalS    int       `db:"healthcheck_interval_s" json:"healthcheck_interval_s"`
	HealthcheckTimeoutS     int       `db:"healthcheck_timeout_s" json:"healthcheck_timeout_s"`
	HealthcheckRetries      int       `db:"healthcheck_retries" json:"healthcheck_retries"`
	HealthcheckStartPeriodS int       `db:"healthcheck_start_period_s" json:"healthcheck_start_period_s"`
	CPULimit                float64   `db:"cpu_limit" json:"cpu_limit"`
	MemLimitMB              int       `db:"mem_limit_mb" json:"mem_limit_mb"`
	User                    string    `db:"user" json:"user"`
	ShmSizeMB               int       `db:"shm_size_mb" json:"shm_size_mb"`
	Privileged              bool      `db:"privileged" json:"privileged"`
	Devices                 string    `db:"devices" json:"devices"`
	RestartPolicy           string    `db:"restart_policy" json:"restart_policy"`
	DependsOn               string    `db:"depends_on" json:"depends_on"`
	Files                   string    `db:"files" json:"files"`
	SharedNet               string    `db:"shared_net" json:"shared_net"`
	Replicas                int       `db:"replicas" json:"replicas"`
	UpdatePolicy            string    `db:"update_policy" json:"update_policy"`
	TagPolicy               string    `db:"tag_policy" json:"tag_policy"`
	CreatedAt               time.Time `db:"created_at" json:"created_at"`
	UpdatedAt               time.Time `db:"updated_at" json:"updated_at"`
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
	ID         string     `db:"id" json:"id"`
	Ref        string     `db:"ref" json:"ref"`
	Digest     string     `db:"digest" json:"digest"`
	BuiltAt    *time.Time `db:"built_at" json:"built_at"`
	LastDigest string     `db:"last_digest" json:"last_digest"`
	LastTag    string     `db:"last_tag" json:"last_tag"`
	LastError  string     `db:"last_error" json:"last_error"`
	CheckedAt  *time.Time `db:"checked_at" json:"checked_at"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
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
	ID            string    `db:"id" json:"id"`
	TileID        string    `db:"tile_id" json:"tile_id"`
	Host          string    `db:"host" json:"host"`
	Path          string    `db:"path" json:"path"`
	ContainerPort int       `db:"container_port" json:"container_port"`
	HTTPS         bool      `db:"https" json:"https"`
	ForceHTTPS    bool      `db:"force_https" json:"force_https"`
	RedirectTo    string    `db:"redirect_to" json:"redirect_to"`
	Auto          bool      `db:"auto" json:"auto"`
	Position      int       `db:"position" json:"position"`
	ProxyJSON     string    `db:"proxy_json" json:"proxy_json"`
	RawCaddy      string    `db:"raw_caddy" json:"raw_caddy"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
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
