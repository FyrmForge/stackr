package deploy

import (
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// resolved is every fact spec needs that the row does not hold.
type resolved struct {
	name, image string
	env, binds  []string
	cmd         []string
	ports       map[string]string
	devices     []docker.Device
	networks    []docker.NetAttach
	cpu         float64
	memMB       int
}

// spec arranges a tile row and its resolved facts into a container. No store
// call, no Docker call: everything arrives as an argument.
func spec(t store.Tile, r resolved) docker.ContainerSpec {
	s := docker.ContainerSpec{
		Name:               r.name,
		Image:              r.image,
		Cmd:                r.cmd,
		Env:                r.env,
		Volumes:            r.binds,
		Ports:              r.ports,
		Networks:           r.networks,
		CPULimit:           r.cpu,
		MemLimitMB:         r.memMB,
		User:               t.User,
		ShmSizeMB:          t.ShmSizeMB,
		Privileged:         t.Privileged,
		Devices:            r.devices,
		Restart:            restart(t.RestartPolicy),
		HealthCmd:          t.HealthcheckCmd,
		HealthIntervalS:    t.HealthcheckIntervalS,
		HealthTimeoutS:     t.HealthcheckTimeoutS,
		HealthRetries:      t.HealthcheckRetries,
		HealthStartPeriodS: t.HealthcheckStartPeriodS,
	}
	// Docker's 30s default would hold the deploy gate half a minute for the
	// first check.
	if s.HealthCmd != "" && s.HealthIntervalS == 0 {
		s.HealthIntervalS = 5
	}
	return s
}

// restart maps the row's canonical word onto docker's. "always" is
// unless-stopped: a replica someone stopped by hand stays stopped when the
// daemon restarts.
func restart(p string) string {
	if p == "" || p == "always" {
		return "unless-stopped"
	}
	return p
}

// publishedPorts parses "host:container[/udp]" lines. It cuts on the first
// ":" only, so "/udp" rides along in the container value for the wrapper. A
// bad line is a warning, never fatal: one typo must not block a deploy.
func publishedPorts(s string) (map[string]string, []string) {
	lines := tile.Lines(s)
	if len(lines) == 0 {
		return nil, nil
	}
	out, warn := map[string]string{}, []string(nil)
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			continue
		}
		host, cont, ok := strings.Cut(l, ":")
		if !ok || host == "" || cont == "" {
			warn = append(warn, fmt.Sprintf("skipping bad published port %q (want host:container[/udp])", l))
			continue
		}
		out[host] = cont
	}
	return out, warn
}

// splitCommand tokenizes a command into argv like a shell word-splits, with
// no expansion and no "sh -c": Docker appends Cmd to the entrypoint, and
// images like prometheus take flags on their own binary.
func splitCommand(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inWord, escaped, quote := false, false, rune(0)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				argv = append(argv, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape in %q", s)
	}
	if inWord {
		argv = append(argv, cur.String())
	}
	return argv, nil
}
