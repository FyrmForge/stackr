package docker

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

// Run creates and starts a container from a resolved spec and returns its id.
func (d *Client) Run(ctx context.Context, spec ContainerSpec) (string, error) {
	labels := map[string]string{LabelManaged: "true"}
	maps.Copy(labels, spec.Labels)

	cfg := &container.Config{
		Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, User: spec.User, Labels: labels,
	}
	if spec.HealthCmd != "" {
		cfg.Healthcheck = &container.HealthConfig{
			Test:        []string{"CMD-SHELL", spec.HealthCmd},
			Interval:    time.Duration(spec.HealthIntervalS) * time.Second,
			Timeout:     time.Duration(spec.HealthTimeoutS) * time.Second,
			Retries:     spec.HealthRetries,
			StartPeriod: time.Duration(spec.HealthStartPeriodS) * time.Second,
		}
	}
	devices := make([]container.DeviceMapping, 0, len(spec.Devices))
	for _, dev := range spec.Devices {
		devices = append(devices, container.DeviceMapping{
			PathOnHost: dev.Host, PathInContainer: dev.Container, CgroupPermissions: dev.Perms,
		})
	}
	hostCfg := &container.HostConfig{
		Binds:         spec.Volumes,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(spec.Restart)},
		Privileged:    spec.Privileged,
		CapAdd:        spec.CapAdd,
		ShmSize:       int64(spec.ShmSizeMB) << 20,
		Resources: container.Resources{
			NanoCPUs: int64(spec.CPULimit * 1e9),
			Memory:   int64(spec.MemLimitMB) << 20,
			Devices:  devices,
		},
	}
	var netCfg *network.NetworkingConfig
	if spec.HostNetwork {
		hostCfg.NetworkMode = network.NetworkHost
	} else {
		cfg.ExposedPorts, hostCfg.PortBindings = portBindings(spec.Ports)
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
		for _, n := range spec.Networks {
			netCfg.EndpointsConfig[n.Name] = &network.EndpointSettings{Aliases: n.Aliases}
		}
		if len(spec.Networks) > 0 {
			// Without this docker also joins the default bridge.
			hostCfg.NetworkMode = container.NetworkMode(spec.Networks[0].Name)
		}
	}

	resp, err := d.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, spec.Name)
	if err != nil {
		return "", wrap(err)
	}
	// A container that cannot start is not left lying around under a name the
	// retry will collide with.
	if err := d.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), resp.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return resp.ID, nil
}

func portBindings(ports map[string]string) (nat.PortSet, nat.PortMap) {
	if len(ports) == 0 {
		return nil, nil
	}
	exposed, bindings := nat.PortSet{}, nat.PortMap{}
	for host, cont := range ports {
		if !strings.Contains(cont, "/") {
			cont += "/tcp" // nat.Port needs the proto or the binding is ignored
		}
		p := nat.Port(cont)
		exposed[p] = struct{}{}
		bindings[p] = append(bindings[p], nat.PortBinding{HostIP: "0.0.0.0", HostPort: host})
	}
	return exposed, bindings
}

// Stop and Restart use the container's own stop timeout (docker's 10s unless
// the image set one).
// ponytail: no per-call grace period; add a timeout arg when a tile gets one.
func (d *Client) Stop(ctx context.Context, id string) error {
	return wrap(d.cli.ContainerStop(ctx, id, container.StopOptions{}))
}

func (d *Client) Start(ctx context.Context, id string) error {
	return wrap(d.cli.ContainerStart(ctx, id, container.StartOptions{}))
}

// Restart keeps networks and addresses, unlike a recreate.
func (d *Client) Restart(ctx context.Context, id string) error {
	return wrap(d.cli.ContainerRestart(ctx, id, container.StopOptions{}))
}

// StopRemove stops (ignoring the error) then force-removes.
func (d *Client) StopRemove(ctx context.Context, id string) error {
	_ = d.cli.ContainerStop(ctx, id, container.StopOptions{})
	return wrap(d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}))
}

// Pause is SIGSTOP via the cgroup: files stop changing, which makes a tar of
// the volume a point-in-time copy.
func (d *Client) Pause(ctx context.Context, id string) error {
	return wrap(d.cli.ContainerPause(ctx, id))
}

// Unpause swallows "not paused": a backup that failed mid-tar still ends here.
func (d *Client) Unpause(ctx context.Context, id string) error {
	err := d.cli.ContainerUnpause(ctx, id)
	if err != nil && strings.Contains(err.Error(), "not paused") {
		return nil
	}
	return wrap(err)
}

// List returns every container (running or not) carrying all of labels.
func (d *Client) List(ctx context.Context, labels map[string]string) ([]Container, error) {
	cs, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: labelFilter(labels)})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(cs))
	for _, c := range cs {
		row := Container{ID: c.ID, Image: c.Image, State: c.State, Health: healthWord(c.Status), Labels: c.Labels}
		if len(c.Names) > 0 {
			row.Name = strings.TrimPrefix(c.Names[0], "/")
		}
		if c.NetworkSettings != nil {
			for _, n := range c.NetworkSettings.Networks {
				if n != nil && n.IPAddress != "" {
					row.IPs = append(row.IPs, n.IPAddress)
				}
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func labelFilter(labels map[string]string) filters.Args {
	f := filters.NewArgs()
	for k, v := range labels {
		f.Add("label", k+"="+v)
	}
	return f
}

// healthWord reads health off the list summary's status string ("Up 2 minutes
// (unhealthy)"): no second inspect per container just to colour a row.
func healthWord(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// Inspect is the live read: state, health, restart count and one IP per
// network, all from one call.
func (d *Client) Inspect(ctx context.Context, id string) (Detail, error) {
	info, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return Detail{}, wrap(err)
	}
	det := Detail{ID: info.ID, Name: strings.TrimPrefix(info.Name, "/"), RestartCount: info.RestartCount,
		Networks: map[string]string{}}
	if info.Config != nil {
		det.Image = info.Config.Image
	}
	if st := info.State; st != nil {
		det.State, det.Started, det.Running = st.Status, st.StartedAt, st.Running
		if st.Health != nil {
			det.Health = st.Health.Status
		}
	}
	if ns := info.NetworkSettings; ns != nil {
		for port, binds := range ns.Ports {
			for _, b := range binds {
				det.Ports = append(det.Ports, b.HostIP+":"+b.HostPort+" -> "+string(port))
			}
		}
		for name, n := range ns.Networks {
			if n != nil {
				det.Networks[name] = n.IPAddress
			}
		}
	}
	for _, m := range info.Mounts {
		src := m.Source
		if m.Name != "" {
			src = m.Name
		}
		det.Mounts = append(det.Mounts, src+" -> "+m.Destination)
	}
	return det, nil
}
