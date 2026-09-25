package api

import (
	"net/http"

	"github.com/FyrmForge/stackr/internal/api/handler/v1"
	"github.com/FyrmForge/stackr/internal/authz"
)

// Gates that are not an authz verb.
const (
	Self   authz.Verb = "@self"   // any live principal: /me, the org list
	Public authz.Verb = "@public" // no principal: exchange, invites
)

// Route is one /api/v1 endpoint. Verb is the authz verb the middleware
// checks, or Self / Public; a route without one does not mount.
type Route struct {
	Method, Path, Op string
	Verb             authz.Verb
	E                v1.Endpoint
}

const (
	org   = "/orgs/:org"
	stack = org + "/stacks/:stack"
	env   = stack + "/envs/:env"
	tile  = env + "/tiles/:tile"
)

// Routes is the whole API, the table the OpenAPI dump and the CLI coverage
// test read. Verb levels live in authz; handlers never check.
func Routes(h *v1.H) []Route {
	return append(routes(h), scoped(h)...)
}

func routes(h *v1.H) []Route {
	const (
		GET, POST, PUT, PATCH, DELETE = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
	)
	return []Route{
		// the caller
		{GET, "/me", "me.get", Self, h.Me()},
		{PUT, "/me/password", "me.password", Self, h.ChangePassword()},
		{GET, "/me/keys", "key.list", Self, h.Keys()},
		{DELETE, "/me/keys/:key", "key.revoke", Self, h.RevokeKey()},
		{GET, "/settings", "settings.catalogue", Self, h.Settings()},
		{POST, "/auth/exchange", "key.exchange", Public, h.ExchangeCLICode()},
		{GET, "/invites/:token", "invite.get", Public, h.LookupInvite()},
		{POST, "/invites/:token/accept", "invite.accept", Self, h.AcceptInvite()},

		// orgs
		{GET, "/orgs", "org.list", Self, h.Orgs()},
		{POST, "/orgs", "org.create", "org.create", h.CreateOrg()},
		{GET, org, "org.get", "org.read", h.GetOrg()},
		{PUT, org + "/name", "org.rename", "org.write", h.RenameOrg()},
		{POST, org + "/finish", "org.finish", "org.write", h.FinishOrg()},
		{DELETE, org, "org.delete", "org.write", h.DeleteOrg()},
		{GET, org + "/members", "member.list", "member.list", h.Members()},
		{PUT, org + "/members/:user", "member.role", "member.manage", h.SetRole()},
		{DELETE, org + "/members/:user", "member.remove", "member.manage", h.RemoveMember()},
		{GET, org + "/invites", "invite.list", "member.manage", h.Invites()},
		{POST, org + "/invites", "invite.create", "member.manage", h.Invite()},
		{POST, org + "/keys", "key.mint", "org.read", h.MintKey()},
		{POST, org + "/cli-codes", "key.cli_code", "org.read", h.CLICode()},
		{GET, org + "/credentials", "credential.list", "org.read", h.Credentials()},
		{POST, org + "/credentials", "credential.create", "registry.credential.write", h.CreateCredential()},
		{PUT, org + "/credentials/:credential", "credential.update", "registry.credential.write", h.UpdateCredential()},
		{DELETE, org + "/credentials/:credential", "credential.delete", "registry.credential.write", h.DeleteCredential()},
		{GET, org + "/connectors", "connector.list", "org.read", h.Connectors()},
		{POST, org + "/connectors", "connector.begin", "connector.write", h.BeginConnector()},
		{PUT, org + "/connectors/:connector/name", "connector.rename", "connector.write", h.RenameConnector()},
		{DELETE, org + "/connectors/:connector", "connector.delete", "connector.write", h.DeleteConnector()},
		{GET, org + "/backup-dests", "dest.list", "org.read", h.BackupDests()},
		{POST, org + "/backup-dests", "dest.create", "destination.write", h.CreateBackupDest()},
		{PUT, org + "/backup-dests/:dest", "dest.update", "destination.write", h.UpdateBackupDest()},
		{DELETE, org + "/backup-dests/:dest", "dest.delete", "destination.write", h.DeleteBackupDest()},
		{GET, org + "/jobs/:job", "job.get", "deployment.read", h.GetJob()},
		{GET, org + "/jobs/:job/log", "job.log", "deployment.read", h.PollJob()},
		{GET, org + "/jobs/:job/events", "job.events", "deployment.read", h.JobEvents()},
		{POST, org + "/jobs/:job/cancel", "job.cancel", "deployment.cancel", h.CancelJob()},
		{DELETE, org + "/slices/:provision", "slice.detach", "tile.write", h.DetachSlice()},

		// volumes and backups, by id under the org
		{DELETE, org + "/volumes/:volume", "volume.delete", "tile.write", h.DeleteVolume()},
		{GET, org + "/volumes/:volume/methods", "backup.methods", "org.read", h.BackupMethods()},
		{GET, org + "/volumes/:volume/schedules", "schedule.list", "org.read", h.BackupSchedules()},
		{POST, org + "/volumes/:volume/schedules", "schedule.create", "backup.write", h.AddBackupSchedule()},
		{PUT, org + "/schedules/:schedule", "schedule.update", "backup.write", h.UpdateBackupSchedule()},
		{DELETE, org + "/schedules/:schedule", "schedule.delete", "backup.write", h.DeleteBackupSchedule()},
		{GET, org + "/volumes/:volume/runs", "backup.list", "org.read", h.BackupRuns()},
		{POST, org + "/volumes/:volume/backups", "backup.now", "backup.write", h.BackupNow()},
		{POST, org + "/volumes/:volume/restore", "backup.restore", "backup.write", h.RestoreBackup()},

		// stacks
		{GET, org + "/stacks", "stack.list", "org.read", h.Stacks()},
		{POST, org + "/stacks", "stack.create", "stack.create", h.CreateStack()},
		{GET, stack, "stack.get", "org.read", h.GetStack()},
		{PUT, stack + "/name", "stack.rename", "stack.write", h.RenameStack()},
		{PUT, stack + "/config-repo", "stack.config-repo", "stack.write", h.SetConfigRepo()},
		{PUT, stack + "/reservations", "stack.reservations", "stack.write", h.SetReservations()},
		{PUT, stack + "/settings", "stack.settings", "stack.write", h.SetStackSettings()},
		{DELETE, stack, "stack.delete", "stack.write", h.DeleteStack()},
		{POST, stack + "/image-check", "stack.image-check", "stack.write", h.CheckStackImages()},
		{GET, stack + "/releases", "release.list", "org.read", h.Releases()},
		{GET, stack + "/releases/:release", "release.get", "org.read", h.Release()},
		{GET, stack + "/envs", "env.list", "org.read", h.Envs()},
		{GET, stack + "/ladder", "env.ladder", "org.read", h.Ladder()},
		{POST, stack + "/envs", "env.create", "env.write", h.CreateEnv()},
		{PUT, stack + "/envs-order", "env.reorder", "env.write", h.ReorderEnvs()},

		// envs
		{GET, env, "env.get", "org.read", h.GetEnv()},
		{PUT, env + "/name", "env.rename", "env.write", h.RenameEnv()},
		{PUT, env + "/color", "env.color", "env.write", h.SetEnvColor()},
		{PUT, env + "/from", "env.from", "env.write", h.SetEnvFrom()},
		{PUT, env + "/settings", "env.settings", "env.write", h.SetEnvSettings()},
		{DELETE, env, "env.delete", "env.write", h.DeleteEnv()},
		{GET, env + "/plan/:release", "promote.plan", "deployment.read", h.PlanPromote()},
		{POST, env + "/promote/:release", "promote.run", "env.write", h.Promote()},
		{POST, env + "/rollback/:release", "promote.rollback", "env.write", h.Rollback()},
		{GET, env + "/tiles", "tile.list", "tile.read", h.Tiles()},
		{POST, env + "/tiles", "tile.create", "tile.write", h.CreateTile()},
		{GET, env + "/managed", "managed.list", "tile.read", h.ManagedInstances()},
		{POST, env + "/managed", "managed.create", "tile.write", h.CreateManagedTile()},

		// tiles
		{GET, tile, "tile.get", "tile.read", h.GetTile()},
		{PATCH, tile, "tile.update", "tile.write", h.UpdateTile()},
		{PUT, tile + "/name", "tile.rename", "tile.write", h.RenameTile()},
		{DELETE, tile, "tile.delete", "tile.write", h.DeleteTile()},
		{POST, tile + "/deploy", "tile.deploy", "tile.write", h.Deploy()},
		{POST, tile + "/restart", "tile.restart", "tile.write", h.RestartTile()},
		{POST, tile + "/stop", "tile.stop", "tile.write", h.StopTile()},
		{POST, tile + "/start", "tile.start", "tile.write", h.StartTile()},
		{POST, tile + "/image-check", "tile.image-check", "tile.write", h.CheckTileImages()},
		{GET, tile + "/status", "tile.status", "tile.read", h.TileStatus()},
		{GET, tile + "/logs", "tile.logs", "tile.read", h.Logs()},
		{GET, tile + "/logs/stream", "tile.logs-stream", "tile.read", h.LogStream()},
		{POST, tile + "/exec", "tile.exec", "container.admin", h.Exec()},
		{GET, tile + "/jobs", "tile.jobs", "tile.read", h.TileJobs()},
		{POST, tile + "/run", "tile.run", "tile.write", h.RunTile()},
		{POST, tile + "/pause", "tile.pause", "tile.write", h.PauseTile()},
		{GET, tile + "/runs", "run.list", "tile.read", h.Runs()},
		{GET, tile + "/runs/:run", "run.get", "tile.read", h.Run()},
		{DELETE, tile + "/runs/:run", "run.stop", "tile.write", h.StopRun()},
		{GET, tile + "/domains", "domain.list", "tile.read", h.Domains()},
		{POST, tile + "/domains", "domain.attach", "domain.write", h.AttachDomain()},
		{PUT, tile + "/domains/:domain", "domain.update", "domain.write", h.UpdateDomain()},
		{PUT, tile + "/domains/:domain/raw-caddy", "domain.raw-caddy", "proxy.admin", h.SetRawCaddy()},
		{DELETE, tile + "/domains/:domain", "domain.detach", "domain.write", h.DetachDomain()},
		{GET, tile + "/slices", "slice.list", "tile.read", h.Slices()},
		{POST, tile + "/slices", "slice.attach", "tile.write", h.AttachSlice()},
		{PUT, tile + "/scope", "managed.scope", "tile.write", h.SetInstanceScope()},

		// admin
		{GET, "/admin/orgs", "admin.orgs", "admin.read", h.AllOrgs()},
		{GET, "/admin/users", "admin.users", "user.admin", h.Users()},
		{PUT, "/admin/users/:user/admin", "admin.user-admin", "user.admin", h.SetAdmin()},
		{POST, "/admin/users/:user/disable", "admin.user-disable", "user.admin", h.DisableUser()},
		{PUT, "/admin/users/:user/password", "admin.user-password", "user.admin", h.SetPassword()},
		{POST, "/admin/keys", "admin.key-mint", "user.admin", h.MintUnboundKey()},
		{GET, "/admin/images", "admin.images", "admin.read", h.Images()},
		{POST, "/admin/image-check", "admin.image-check", "container.admin", h.CheckAllImages()},
		{GET, "/admin/jobs", "admin.jobs", "admin.read", h.Jobs()},
		{GET, "/admin/jobs/:job", "admin.job-get", "admin.read", h.GetJob()},
		{GET, "/admin/jobs/:job/log", "admin.job-log", "admin.read", h.PollJob()},
		{GET, "/admin/jobs/:job/events", "admin.job-events", "admin.read", h.JobEvents()},
		{POST, "/admin/jobs/:job/cancel", "admin.job-cancel", "container.admin", h.CancelJob()},
		{GET, "/admin/version", "admin.version", "admin.read", h.Version()},
		{GET, "/admin/upgrade", "admin.upgrade-check", "admin.read", h.CheckUpgrade()},
		{POST, "/admin/upgrade", "admin.upgrade", "container.admin", h.Upgrade()},
		{GET, "/admin/panel-backups", "admin.panel-backups", "admin.read", h.PanelBackups()},
		{POST, "/admin/panel-backups", "admin.panel-backup", "container.admin", h.PanelBackupNow()},
		{POST, "/admin/proxy/sync", "admin.proxy-sync", "proxy.admin", h.SyncProxy()},
		{GET, "/admin/settings/:setting", "admin.setting-get", "admin.read", h.Setting()},
		{PUT, "/admin/settings/:setting", "admin.setting-set", "serverdefaults.set", h.SetSetting()},
		{GET, "/admin/defaults", "admin.defaults", "admin.read", h.SettingDefaults()},
		{PATCH, "/admin/defaults", "admin.defaults-set", "serverdefaults.set", h.SetSettingDefaults()},
		{GET, "/admin/backup-dests", "admin.dest-list", "admin.read", h.GlobalBackupDests()},
		{POST, "/admin/backup-dests", "admin.dest-create", "serverdefaults.set", h.CreateGlobalBackupDest()},
		{PUT, "/admin/backup-dests/:dest", "admin.dest-update", "serverdefaults.set", h.UpdateGlobalBackupDest()},
		{DELETE, "/admin/backup-dests/:dest", "admin.dest-delete", "serverdefaults.set", h.DeleteGlobalBackupDest()},
	}
}

// scoped are the routes that exist once per params/volumes scope.
func scoped(h *v1.H) []Route {
	var out []Route
	for _, s := range []struct {
		base, name string
		at         v1.At
	}{{org, "org", v1.AtOrg}, {stack, "stack", v1.AtStack}, {env, "env", v1.AtEnv}} {
		out = append(out,
			Route{http.MethodGet, s.base + "/params", s.name + ".params", "tile.read", h.Params(s.at)},
			Route{http.MethodGet, s.base + "/params/secrets", s.name + ".secrets", "variable.write", h.Secrets(s.at)},
			Route{http.MethodPatch, s.base + "/params", s.name + ".params-set", "variable.write", h.SetParams(s.at)},
			Route{http.MethodDelete, s.base + "/params/:collection/:name", s.name + ".param-delete", "variable.write", h.DeleteParam(s.at)},
			Route{http.MethodGet, s.base + "/volumes", s.name + ".volumes", "org.read", h.Volumes(s.at)},
			Route{http.MethodPost, s.base + "/volumes", s.name + ".volume-declare", "tile.write", h.DeclareVolume(s.at)},
		)
	}
	return out
}
