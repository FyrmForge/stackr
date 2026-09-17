package stackconf

import (
	"sort"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// StateToResolved serializes a live State (the current DB tiles) into a
// Resolved config that diffs clean against that same State. It is the basis
// for UI staging: the committed desired state is *derivable* from the tiles,
// staged edits patch this Resolved, and the existing Diff/execute engine
// reconciles, no second reconcile path.
//
// It is the read-side inverse of applyTileConf; the two must stay in sync or a
// live env won't round-trip to an empty diff (see TestStateToResolvedRoundTrip).
//
// multi-line quoted env values don't round-trip exactly because
// canonEnv (the differ's env comparison) is lossy for them. Single-line env,
// the overwhelming norm, is exact. The fix belongs in canonEnv, not here.
func StateToResolved(stackName string, s State) *Resolved {
	order := make([]string, 0, len(s.Envs))
	for name := range s.Envs {
		if name != repo.HomeSlug { // shared tiles: in Envs, not on the ladder
			order = append(order, name)
		}
	}
	sort.Strings(order) // stable; real display order is applied by the caller
	r := &Resolved{Stack: stackName, EnvOrder: order, Envs: map[string]ResolvedEnv{},
		Middlewares: ParseMiddlewares(s.Middlewares)}
	// Domain resources round-trip as-is: a staged UI edit builds its desired
	// config from this, and an empty Domains would read as "delete them all".
	for _, d := range s.DomainRes {
		r.Domains = append(r.Domains, DomainResConf{Host: d.Host, ACMEEmail: d.ACMEEmail,
			IncludeEnvOnDefault: d.IncludeEnvOnDefault})
	}
	for name, es := range s.Envs {
		re := ResolvedEnv{Tiles: map[string]TileConf{}}
		for slug, ts := range es.Tiles {
			if ts.Slice != nil {
				re.Tiles[slug] = sliceToConf(slug, ts.Slice)
				continue
			}
			re.Tiles[slug] = tileToConf(ts, es)
		}
		r.Envs[name] = re
	}
	return r
}

// sliceToConf serializes a live slice back into its config form, the
// read-side mirror of the slice apply path; every field set here is one
// diffSlice compares.
func sliceToConf(slug string, sl *SliceState) TileConf {
	tc := TileConf{Type: "slice", From: sl.InstancePath, Public: sl.Public}
	if sl.Name != slug {
		tc.Name = sl.Name
	}
	if removalPolicy(sl.OnRemove) == "drop" {
		tc.OnRemove = "drop"
	}
	return tc
}

// TileConfOf serializes a single tile to its config form, the exported entry
// the UI-staging handlers use to build a settings patch from an edited tile.
// cur supplies the env's tiles for volume attach resolution (pass a zero
// EnvState for services/crons, which don't need it).
func TileConfOf(ts TileState, cur EnvState) TileConf { return tileToConf(ts, cur) }

// tileToConf serializes one live tile back into its TileConf, the read-side
// mirror of applyTileConf. Every field set here is one diffTile compares.
func tileToConf(ts TileState, cur EnvState) TileConf {
	t := ts.Tile
	var tc TileConf
	switch {
	case t.IsVolume():
		tc.Type = "volume"
		tc.Attach = attachedSlug(cur, t.AttachedTileID)
		tc.Path = t.MountPath
	case t.IsManaged():
		tc.Type = "managed"
		tc.Engine = t.Engine
		tc.ExternalPort = t.ExternalPort
		tc.ShmSizeMB = t.ShmSizeMB
		if t.Replicas > 1 {
			tc.Replicas = t.Replicas
		}
		tc.NodeGroup = t.NodeGroup
		// Only an override is worth carrying; the engine default would pin the
		// file to today's default and turn every stackr upgrade into a diff.
		if eng, ok := managedtiles.Engines[t.Engine]; ok && t.ImageRef != eng.DefaultImage {
			tc.Image = t.ImageRef
		}
		// "env" is the default, so emitting it would be noise; anything else is
		// a real choice the file has to carry or the first apply undoes it.
		if t.ScopeKind != "" && t.ScopeKind != "env" {
			tc.Scope = t.ScopeKind
		}
	case t.Kind == "cron":
		tc.Type = "cron"
		sourceToConf(&t, &tc)
		tc.Schedule = t.Cron
		tc.Command = t.Command
		tc.AllowOverlap = t.AllowOverlap
		tc.TimeoutMinutes = t.TimeoutMinutes
	case t.Kind == "function":
		tc.Type = "function"
		sourceToConf(&t, &tc)
		tc.Command = t.Command
		tc.RunOnDeploy = t.RunOnDeploy
		tc.AllowOverlap = t.AllowOverlap
		tc.TimeoutMinutes = t.TimeoutMinutes
	default:
		tc.Type = "service"
		sourceToConf(&t, &tc)
		tc.Port = t.ContainerPort
		tc.Healthcheck = t.HealthcheckCmd
		tc.HealthInterval = t.HealthcheckIntervalS
		tc.HealthTimeout = t.HealthcheckTimeoutS
		tc.HealthRetries = t.HealthcheckRetries
		tc.HealthStartPeriod = t.HealthcheckStartPeriodS
		tc.Files = FileList(splitTileLines(t.Files))
		tc.Storage = splitTileLines(t.Storage)
		tc.SecurityHeaders = t.SecHeaders
		tc.Volumes = splitTileLines(t.Volumes)
		tc.WatchPaths = splitTileLines(t.WatchPaths)
		tc.BuildArgs = t.BuildArgs
		tc.PublishedPorts = t.PublishedPorts
		tc.TraefikOverride = t.TraefikOverride
		tc.BasicAuthUser = t.BasicAuthUser
		tc.BasicAuthPassword = t.BasicAuthPassword
		tc.Command = t.Command
		tc.User = t.User
		tc.ShmSizeMB = t.ShmSizeMB
		if t.Replicas > 1 {
			tc.Replicas = t.Replicas
		}
		tc.NodeGroup = t.NodeGroup
		tc.Privileged = t.Privileged
		tc.Devices = splitTileLines(t.Devices)
		tc.Restart = t.RestartPolicy
		tc.Domains = domainsToConf(ts.Domains, cur.ApexHosts, ts.Tile.ContainerPort)
	}
	switch t.Kind {
	case "service", "cron", "function":
		tc.DependsOn = splitTileLines(t.DependsOn)
		tc.WaitForCI = t.WaitForCI
	}
	// "off" is the default; emitting it would pin every file to today's noise.
	if t.UpdatePolicy != "" && t.UpdatePolicy != "off" {
		tc.UpdatePolicy = t.UpdatePolicy
	}
	// diffTile compares limits + env for every kind (when set), so set them
	// always to round-trip; both are no-ops on tiles that don't use them.
	tc.Limits = &LimitsConf{CPU: t.CPULimit, MemoryMB: t.MemLimitMB}
	tc.Env = envToMap(t.Env)
	return tc
}

// sourceToConf serializes a runnable tile's source, the read-side mirror of
// applySource, shared by every run policy.
func sourceToConf(t *repo.Tile, tc *TileConf) {
	switch t.SourceType {
	case "image":
		tc.Image = t.ImageRef
		tc.GitURL = t.GitURL
		tc.Branch = t.GitBranch
		if t.GitURL != "" {
			tc.Connector = t.ConnectorID
		}
	default:
		tc.Branch = t.GitBranch
		tc.GitURL = t.GitURL
		tc.Connector = t.ConnectorID
		tc.Build = &BuildConf{Context: t.BuildContext, Dockerfile: t.DockerfilePath}
	}
}

func splitTileLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// envToMap parses a stored KEY=VALUE blob into the config's env map.
func envToMap(raw string) EnvMap {
	vars := envutil.Parse(raw)
	m := make(EnvMap, len(vars))
	for _, v := range vars {
		m[v.Key] = v.Value
	}
	return m
}

// domainsToConf serializes a tile's domains (rows come position-ordered). A
// nil HTTPS means "on" (the config default), so only an http-only domain
// records an explicit false. Generated rows serialize as the intent (auto),
// resource-host rows as apex claims, the read-side mirror of claimHost.
func domainsToConf(ds []repo.Domain, apexHosts map[string]bool, tilePort int) []DomainConf {
	if len(ds) == 0 {
		return nil
	}
	out := make([]DomainConf, 0, len(ds))
	for _, d := range ds {
		if d.Auto {
			out = append(out, DomainConf{Auto: true})
			continue
		}
		if apexHosts[d.Host] {
			out = append(out, DomainConf{Apex: d.Host})
			continue
		}
		dc := DomainConf{Host: d.Host, Path: d.Path, RedirectTo: d.RedirectTo,
			Middlewares: d.MiddlewareList(), Priority: d.Priority, Rule: d.Rule}
		if d.ContainerPort != 0 && d.ContainerPort != tilePort && d.RedirectTo == "" {
			dc.Port = d.ContainerPort
		}
		if !d.HTTPS {
			off := false
			dc.HTTPS = &off
		} else if !d.ForceHTTPS {
			// Only meaningful alongside https:, and nil means "on", so an
			// explicit false is the only thing worth recording.
			off := false
			dc.ForceHTTPS = &off
		}
		out = append(out, dc)
	}
	return out
}
