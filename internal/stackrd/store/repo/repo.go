// Package repo is the data layer: the Store interface every handler talks to,
// the row structs behind it, and the slug rules shared by both. The SQLite
// implementation lives in repo/sqlite, nothing outside that subpackage writes
// SQL, so a second backend only has to satisfy Store.
package repo

import (
	"context"
	"database/sql"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
)

// Store defines the data access interface for the application.
type Store interface {
	// Health checks the database connection.
	Health(ctx context.Context) error

	// Session persistence, satisfies auth.SessionStore.
	auth.SessionStore

	// Users
	GetUserByID(ctx context.Context, id string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	CreateUser(ctx context.Context, user *User) error
	UpdateUser(ctx context.Context, user *User) error
	CountUsers(ctx context.Context) (int, error)
	ListUsers(ctx context.Context) ([]User, error)

	// Servers
	GetServer(ctx context.Context, id string) (*Server, error)
	GetServerByNodeID(ctx context.Context, nodeID string) (*Server, error)
	ListServers(ctx context.Context) ([]Server, error)
	CreateServer(ctx context.Context, sv *Server) error
	UpdateServer(ctx context.Context, sv *Server) error
	DeleteServer(ctx context.Context, id string) error

	// Join keys (one-time swarm join secrets)
	CreateJoinKey(ctx context.Context, k *JoinKey) error
	GetJoinKey(ctx context.Context, key string) (*JoinKey, error)
	LatestJoinKey(ctx context.Context, serverID string) (*JoinKey, error)
	BurnJoinKey(ctx context.Context, key string) (bool, error)
	BurnServerJoinKeys(ctx context.Context, serverID string) (int, error)

	// Orgs
	GetOrg(ctx context.Context, id string) (*Org, error)
	GetOrgBySlug(ctx context.Context, slug string) (*Org, error)
	ListOrgs(ctx context.Context) ([]Org, error)
	CreateOrg(ctx context.Context, o *Org) error
	UpdateOrg(ctx context.Context, o *Org) error
	DeleteOrg(ctx context.Context, id string) error

	// Org membership + invites
	ListOrgsForUser(ctx context.Context, userID string) ([]Org, error)
	ListOrgMembers(ctx context.Context, orgID string) ([]OrgMember, error)
	GetOrgMember(ctx context.Context, orgID, userID string) (*OrgMember, error)
	UpsertOrgMember(ctx context.Context, m *OrgMember) error
	DeleteOrgMember(ctx context.Context, orgID, userID string) error
	CreateInvite(ctx context.Context, i *Invite) error
	GetInvite(ctx context.Context, id string) (*Invite, error)
	ListInvitesByOrg(ctx context.Context, orgID string) ([]Invite, error)
	MarkInviteUsed(ctx context.Context, id string) error
	DeleteInvite(ctx context.Context, id string) error

	// Connectors
	CreateConnector(ctx context.Context, cn *Connector) error
	GetConnector(ctx context.Context, id string) (*Connector, error)
	ListConnectorsByOrg(ctx context.Context, orgID string) ([]Connector, error)
	ListConnectors(ctx context.Context) ([]Connector, error)
	UpdateConnector(ctx context.Context, cn *Connector) error
	DeleteConnector(ctx context.Context, id string) error

	// Storage (server-scoped shares/pools, §2.7)
	CreateStorage(ctx context.Context, s *Storage) error
	GetStorage(ctx context.Context, id string) (*Storage, error)
	GetStorageBySlug(ctx context.Context, slug string) (*Storage, error)
	GetOrgStorageBySlug(ctx context.Context, orgID, slug string) (*Storage, error)
	ListStorage(ctx context.Context) ([]Storage, error)
	UpdateStorage(ctx context.Context, s *Storage) error
	DeleteStorage(ctx context.Context, id string) error
	CreateStoragePath(ctx context.Context, p *StoragePath) error
	GetStoragePath(ctx context.Context, id string) (*StoragePath, error)
	ListStoragePaths(ctx context.Context, storageID string) ([]StoragePath, error)
	DeleteStoragePath(ctx context.Context, id string) error

	// Stacks
	CreateStack(ctx context.Context, s *Stack) error
	GetStack(ctx context.Context, id string) (*Stack, error)
	GetStackBySlug(ctx context.Context, orgID, slug string) (*Stack, error)
	ListStacks(ctx context.Context) ([]Stack, error)
	ListStacksByOrg(ctx context.Context, orgID string) ([]Stack, error)
	SetStackOrg(ctx context.Context, stackID, orgID string) error
	UpdateStack(ctx context.Context, s *Stack) error
	CreateConfigPlan(ctx context.Context, p *ConfigPlan) error
	GetConfigPlan(ctx context.Context, id string) (*ConfigPlan, error)
	LatestConfigPlan(ctx context.Context, stackID string) (*ConfigPlan, error)
	CountStacksAwaitingPlan(ctx context.Context, orgID string) (int, error)
	ListConfigPlans(ctx context.Context, stackID string, limit int) ([]ConfigPlan, error)
	SetConfigPlanStatus(ctx context.Context, id, status string) error
	// SetConfigPlanError keeps why an apply failed on the row, so the plan
	// page can show it after the flash is gone. The status is untouched: a
	// stack plan that failed to apply is still the pending decision.
	SetConfigPlanError(ctx context.Context, id, msg string) error
	SupersedePendingPlans(ctx context.Context, stackID, envSlug string) error
	CreateOrgConfigPlan(ctx context.Context, p *ConfigPlan) error
	GetOrgConfigPlan(ctx context.Context, id string) (*ConfigPlan, error)
	ListOrgConfigPlans(ctx context.Context, orgID string, limit int) ([]ConfigPlan, error)
	SetOrgConfigPlanStatus(ctx context.Context, id, status string) error
	// SetOrgConfigPlanError marks the plan errored and keeps the reason.
	SetOrgConfigPlanError(ctx context.Context, id, msg string) error
	SupersedePendingOrgPlans(ctx context.Context, orgID string) error
	DeleteStack(ctx context.Context, id string) error

	// Environments
	CreateEnvironment(ctx context.Context, e *Environment) error
	GetEnvironment(ctx context.Context, id string) (*Environment, error)
	GetEnvironmentBySlug(ctx context.Context, stackID, slug string) (*Environment, error)
	UpdateEnvironment(ctx context.Context, e *Environment) error
	RenameEnvironment(ctx context.Context, id, name, slug string) error
	SetEnvironmentProxy(ctx context.Context, envID, ip, cidr string) error
	// SetEnvironmentNetwork records which pooled overlay this env holds
	// ("" returns it). SetTileSharedNet is the same for a shared managed
	// instance's own overlay. ClaimedNetworks is every name either column
	// currently holds, what infra/netpool hands out from and sweeps against.
	SetEnvironmentNetwork(ctx context.Context, envID, name string) error
	SetTileSharedNet(ctx context.Context, tileID, name string) error
	ClaimedNetworks(ctx context.Context) ([]string, error)
	EnvironmentsWithoutNetwork(ctx context.Context) ([]string, error)
	// ListEnvironmentsByStack is the ladder: static envs by position, then
	// ephemeral ones. The home is never in it.
	ListEnvironmentsByStack(ctx context.Context, stackID string) ([]Environment, error)
	// HomeEnvironment is the stack's home for shared tiles (nil if none).
	HomeEnvironment(ctx context.Context, stackID string) (*Environment, error)
	DeleteEnvironment(ctx context.Context, id string) error
	// Intended flags: differences between environments marked as on purpose.
	ListIntended(ctx context.Context, envID string) ([]Intended, error)
	SetIntended(ctx context.Context, row *Intended) error
	DeleteIntended(ctx context.Context, envID, tileSlug, key string) error
	// ClearDeclaredIntended drops the rows the config file wrote (value ""),
	// so an apply can rewrite them from the file as it is now.
	ClearDeclaredIntended(ctx context.Context, envID string) error

	// Tiles
	CreateTile(ctx context.Context, t *Tile) error
	GetTile(ctx context.Context, id string) (*Tile, error)
	GetTileBySlug(ctx context.Context, envID, slug string) (*Tile, error)
	ListTilesByEnv(ctx context.Context, envID string) ([]Tile, error)
	ListTilesByStack(ctx context.Context, stackID string) ([]Tile, error)
	ListTiles(ctx context.Context) ([]Tile, error)
	UpdateTile(ctx context.Context, t *Tile) error
	RenameTile(ctx context.Context, id, name, slug string) error
	UpdateTileStatus(ctx context.Context, id, status string) error
	SetTileHomeNode(ctx context.Context, id, nodeID string) error
	SetTileImageDigest(ctx context.Context, id, digest string) error
	SetTileLatestDigest(ctx context.Context, id, digest string) error
	RecordTileRun(ctx context.Context, id, status, output string) error
	DeleteTile(ctx context.Context, id string) error

	// Deployments
	CreateDeployment(ctx context.Context, d *Deployment) error
	GetDeployment(ctx context.Context, id string) (*Deployment, error)
	ListDeploymentsByTile(ctx context.Context, tileID string, limit int) ([]Deployment, error)
	ListDeploymentsByStatus(ctx context.Context, status string) ([]Deployment, error)

	// Work queue (docs/plans/33-workqueue.md).
	CreateWorkItem(ctx context.Context, w *WorkItem) error
	GetWorkItem(ctx context.Context, id string) (*WorkItem, error)
	ListWorkItemsByStatus(ctx context.Context, status string) ([]WorkItem, error)
	LatestWorkItem(ctx context.Context, kind, dedupeKey string) (*WorkItem, error)
	SupersedeQueuedWorkItems(ctx context.Context, kind, dedupeKey, exceptID string) error
	ClaimWorkItem(ctx context.Context, id string) (bool, error)
	FinishWorkItem(ctx context.Context, id, status, errMsg string) error
	RequeueWorkItem(ctx context.Context, id string) error
	SetWorkItemProgress(ctx context.Context, id, step, progress string) error
	UpdateDeployment(ctx context.Context, d *Deployment) error
	// SweepStaleRuns clears work that only the running process could have
	// finished, call once at startup, after a crash or a restart mid-deploy.
	SweepStaleRuns(ctx context.Context) error

	// Domains
	CreateDomain(ctx context.Context, d *Domain) error
	GetDomain(ctx context.Context, id string) (*Domain, error)
	GetDomainByHostPath(ctx context.Context, host, path string) (*Domain, error)
	ListDomainsByTile(ctx context.Context, tileID string) ([]Domain, error)
	ListDomains(ctx context.Context) ([]Domain, error)
	SetDomainHTTPS(ctx context.Context, id string, https bool) error
	SetDomainForceHTTPS(ctx context.Context, id string, force bool) error
	SetDomainResourceACME(ctx context.Context, id, email string) error
	SetDomainCert(ctx context.Context, id, certPEM, keyPEM string) error
	DeleteDomain(ctx context.Context, id string) error

	// Domain resources
	CreateDomainResource(ctx context.Context, r *DomainResource) error
	UpdateDomainResource(ctx context.Context, r *DomainResource) error
	ListDomainResources(ctx context.Context) ([]DomainResource, error)
	DeleteDomainResource(ctx context.Context, id string) error

	// SSH keys

	// Registries
	CreateRegistry(ctx context.Context, r *Registry) error
	GetRegistry(ctx context.Context, id string) (*Registry, error)
	GetManagedRegistry(ctx context.Context) (*Registry, error)
	ListRegistries(ctx context.Context) ([]Registry, error)
	UpdateRegistry(ctx context.Context, r *Registry) error
	DeleteRegistry(ctx context.Context, id string) error

	// Cron run history
	CreateCronRun(ctx context.Context, r *CronRun) error
	FinishCronRun(ctx context.Context, r *CronRun) error
	ListCronRuns(ctx context.Context, ref string, limit int) ([]CronRun, error)
	GetCronRun(ctx context.Context, id string) (*CronRun, error)
	OpenCronRun(ctx context.Context, ref string) (*CronRun, error)
	ListOpenCronRuns(ctx context.Context) ([]CronRun, error)
	PruneCronRuns(ctx context.Context, before time.Time) error
	CloseOrphanCronRuns(ctx context.Context) error

	// API keys
	CreateOrgRegistryCredential(ctx context.Context, c *OrgRegistryCredential) error
	GetOrgRegistryCredentialByHash(ctx context.Context, hash string) (*OrgRegistryCredential, error)
	GetOrgRegistryCredential(ctx context.Context, id string) (*OrgRegistryCredential, error)
	ListOrgRegistryCredentials(ctx context.Context, orgID string) ([]OrgRegistryCredential, error)
	DeleteOrgRegistryCredential(ctx context.Context, id string) error
	DeleteSystemOrgRegistryCredential(ctx context.Context, id string) error
	TouchOrgRegistryCredential(ctx context.Context, id string) error

	CreateAPIKey(ctx context.Context, k *APIKey) error
	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	ListAPIKeys(ctx context.Context) ([]APIKey, error)
	DeleteAPIKey(ctx context.Context, id string) error

	// UI staging buffer (per-env pending structural edits)
	CreateStagedChange(ctx context.Context, s *StagedChange) error
	ListStagedByEnv(ctx context.Context, envID string) ([]StagedChange, error)
	ListStagedByStack(ctx context.Context, stackID string) ([]StagedChange, error)
	CountStagedByEnv(ctx context.Context, envID string) (int, error)
	GetStagedChange(ctx context.Context, id string) (*StagedChange, error)
	DeleteStagedChange(ctx context.Context, id string) error
	DeleteStagedByEnv(ctx context.Context, envID string) error

	// Shared-instance provisions (passwords decrypted on read)
	CreateProvision(ctx context.Context, p *Provision) error
	GetProvision(ctx context.Context, id string) (*Provision, error)
	ListProvisionsByInstance(ctx context.Context, instanceTileID string) ([]Provision, error)
	ListProvisionsByConsumer(ctx context.Context, consumerTileID string) ([]Provision, error)
	ListProvisionsByEnv(ctx context.Context, envID string) ([]Provision, error)
	UpdateProvision(ctx context.Context, p *Provision) error
	DeleteProvision(ctx context.Context, id string) error

	// Variables (values decrypted on read). ReplaceTileVars is the config-apply
	// write: the file is the whole truth, so undeclared non-secret vars go.
	ReplaceTileVars(ctx context.Context, t *Tile) error
	ListVariables(ctx context.Context, ownerKind, ownerID string) ([]Variable, error)
	// ListVariableNames is the search view: every variable's owner, name and
	// secret flag, with Value left empty, nothing is decrypted.
	ListVariableNames(ctx context.Context) ([]Variable, error)
	UpsertVariable(ctx context.Context, v *Variable) error
	DeleteVariable(ctx context.Context, ownerKind, ownerID, name string) error

	// Secret audit trail: reads and writes of secret variable values.
	// Inserts are best-effort, callers log failures, never abort on them.
	AddAuditEvent(ctx context.Context, e *AuditEvent) error
	ListAuditEvents(ctx context.Context, ownerKind, ownerID string, limit int) ([]AuditEvent, error)
	ListAllAuditEvents(ctx context.Context, actor, action string, limit int) ([]AuditEvent, error)

	// Secret links. ClaimSecretLink is the burn: it flips open -> state in one
	// statement and reports whether this caller was the one that won, so two
	// concurrent submits can't both go through.
	CreateSecretLink(ctx context.Context, l *SecretLink) error
	GetSecretLinkByHash(ctx context.Context, tokenHash string) (*SecretLink, error)
	ListSecretLinks(ctx context.Context, ownerKind, ownerID string) ([]SecretLink, error)
	ClaimSecretLink(ctx context.Context, id, state string) (bool, error)
	// BurnDropLink is the claim plus the variable writes in one transaction:
	// a failed write rolls back the burn, so the sender can always resubmit.
	BurnDropLink(ctx context.Context, id string, vars []Variable) (bool, error)
	TouchSecretLink(ctx context.Context, id string, attempts int, openedAt sql.NullTime) error
	DeleteSecretLink(ctx context.Context, id string) error

	// Managed resources + their outputs and consumer bindings
	// (output values decrypted on read)
	CreateResource(ctx context.Context, r *ManagedResource) error
	GetResource(ctx context.Context, id string) (*ManagedResource, error)
	ListResourcesByEnv(ctx context.Context, envID string) ([]ManagedResource, error)
	ListResourcesByProvider(ctx context.Context, providerTileID string) ([]ManagedResource, error)
	UpdateResource(ctx context.Context, r *ManagedResource) error
	DeleteResource(ctx context.Context, id string) error
	ListOutputs(ctx context.Context, resourceID string) ([]ResourceOutput, error)
	UpsertOutput(ctx context.Context, o *ResourceOutput) error
	CreateBinding(ctx context.Context, b *ResourceBinding) error
	DeleteBinding(ctx context.Context, resourceID, consumerTileID string) error
	ListBindingsByResource(ctx context.Context, resourceID string) ([]ResourceBinding, error)
	BindingsForConsumer(ctx context.Context, consumerTileID string) ([]ResourceBinding, error)

	// Backups (destination secret keys decrypted on read). Nothing here
	// filters by org, callers resolve tile → org → membership first.
	CreateBackupDestination(ctx context.Context, d *BackupDestination) error
	GetBackupDestination(ctx context.Context, id string) (*BackupDestination, error)
	ListBackupDestinations(ctx context.Context) ([]BackupDestination, error)
	UpdateBackupDestination(ctx context.Context, d *BackupDestination) error
	DeleteBackupDestination(ctx context.Context, id string) error
	CreateBackup(ctx context.Context, b *Backup) error
	GetBackup(ctx context.Context, id string) (*Backup, error)
	ListBackupsByTile(ctx context.Context, tileID string) ([]Backup, error)
	ListBackups(ctx context.Context) ([]Backup, error)
	UpdateBackup(ctx context.Context, b *Backup) error
	DeleteBackup(ctx context.Context, id string) error
	CreateBackupRun(ctx context.Context, r *BackupRun) error
	GetBackupRun(ctx context.Context, id string) (*BackupRun, error)
	ListBackupRuns(ctx context.Context, backupID string, limit int) ([]BackupRun, error)
	UpdateBackupRun(ctx context.Context, r *BackupRun) error

	// Settings
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error

	// Notifications, every one belongs to a single recipient.
	CreateNotification(ctx context.Context, n *Notification) error
	ListNotifications(ctx context.Context, userID string, limit int) ([]Notification, error)
	CountUnreadNotifications(ctx context.Context, userID string) (int, error)
	MarkAllNotificationsRead(ctx context.Context, userID string) error
	DeleteAllNotifications(ctx context.Context, userID string) error
	PruneNotifications(ctx context.Context, before time.Time) error

	// Node positions (stack graph canvas, slug-keyed)
	ListNodePositions(ctx context.Context, ownerID string) ([]NodePosition, error)
	UpsertNodePosition(ctx context.Context, p *NodePosition) error
	// SaveNodePositions writes a whole canvas layout in one transaction, after
	// validating it. A partially-applied drag leaves a scrambled canvas, and
	// the payload comes straight off the wire.
	SaveNodePositions(ctx context.Context, ownerID string, ps []NodePosition) error
	DeleteNodePositions(ctx context.Context, ownerID string) error

	// Annotations (canvas notes, shared per canvas like node positions)
	ListAnnotations(ctx context.Context, ownerID string) ([]Annotation, error)
	UpsertAnnotation(ctx context.Context, a *Annotation) error
	DeleteAnnotation(ctx context.Context, ownerID, id string) error

	// Graph groups (cards/annotations that move together, per canvas)
	ListGraphGroups(ctx context.Context, ownerID string) ([]GraphGroup, error)
	UpsertGraphGroup(ctx context.Context, g *GraphGroup) error
	DeleteGraphGroup(ctx context.Context, ownerID, id string) error

	// Metrics
	InsertMetric(ctx context.Context, m *Metric) error
	ListMetrics(ctx context.Context, ref string, since time.Time) ([]Metric, error)
	PruneMetrics(ctx context.Context, before time.Time) error
}
