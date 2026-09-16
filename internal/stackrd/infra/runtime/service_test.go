package runtime

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

// A pinned tile is the one that loses data if this is wrong: exactly one
// replica, and the old task stopped before the new one starts, or two
// containers write the same volume for the length of a rolling update.
func TestPinnedSpec(t *testing.T) {
	spec, err := serviceSpec(ServiceSpec{Name: "db", Image: "postgres:16", Replicas: 3,
		Pinned: true, HomeNode: "node-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.TaskTemplate.Placement.Constraints[0]; got != "node.id == node-1" {
		t.Fatalf("pinned constraint: %q", got)
	}
	if got := *spec.Mode.Replicated.Replicas; got != 1 {
		t.Fatalf("pinned replicas: %d, want 1", got)
	}
	if spec.UpdateConfig.Order != swarm.UpdateOrderStopFirst {
		t.Fatalf("pinned order: %s", spec.UpdateConfig.Order)
	}
	if spec.TaskTemplate.Placement == nil || len(spec.TaskTemplate.Placement.Constraints) == 0 {
		t.Fatal("pinned tile has no placement constraint")
	}
	// Stateless keeps the caller's replicas and rolls the other way.
	spec, _ = serviceSpec(ServiceSpec{Name: "web", Image: "nginx", Replicas: 3})
	if got := *spec.Mode.Replicated.Replicas; got != 3 {
		t.Fatalf("stateless replicas: %d, want 3", got)
	}
	if spec.UpdateConfig.Order != swarm.UpdateOrderStartFirst {
		t.Fatalf("stateless order: %s", spec.UpdateConfig.Order)
	}
	if spec.TaskTemplate.Placement != nil {
		t.Fatal("stateless tile must not be pinned to a node")
	}
}

// A pinned tile with no home node must not produce a spec at all. Swarm would
// place it on any node, start it against an empty volume, and report a
// healthy task, silent data loss, not an error
// (docs/plans/30-docker-swarm.md, addendum).
func TestPinnedWithoutHomeNodeIsRefused(t *testing.T) {
	if _, err := serviceSpec(ServiceSpec{Name: "db", Image: "postgres:16", Pinned: true}); err == nil {
		t.Fatal("a pinned tile with no home node produced a spec")
	}
	// The manager-pinned infrastructure services are the exception: they are
	// placed by role, not by which disk holds their data.
	if _, err := serviceSpec(ServiceSpec{Name: "stkr-traefik", Image: "traefik",
		Pinned: true, ManagerOnly: true}); err != nil {
		t.Fatalf("manager-pinned service refused: %v", err)
	}
}

// The node group is a constraint against the docker node label, and the
// label prefix matters: "stackr.group == x" without "node.labels." silently
// matches nothing and the task never schedules.
func TestNodeGroupConstraint(t *testing.T) {
	spec, err := serviceSpec(ServiceSpec{Name: "web", Image: "nginx", NodeGroup: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.TaskTemplate.Placement.Constraints[0]; got != "node.labels.stackr.group == gpu" {
		t.Fatalf("group constraint: %q", got)
	}
}

// The agent is the one global service: one task per node, placed by swarm,
// with the shared key mounted where the binary looks for it.
func TestGlobalServiceWithSecret(t *testing.T) {
	spec, err := serviceSpec(ServiceSpec{Name: "stkr-agent", Image: "stkr:local", Global: true,
		Secrets: []SecretMount{{ID: "sid", Name: "stkr-agent-key", Target: "agent_key"}}})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Mode.Global == nil || spec.Mode.Replicated != nil {
		t.Fatal("global service is not in global mode")
	}
	secs := spec.TaskTemplate.ContainerSpec.Secrets
	if len(secs) != 1 || secs[0].File.Name != "agent_key" {
		t.Fatalf("secret mount: %+v", secs)
	}
}

// Labels belong on the container spec. Service labels never reach the task's
// container, and ListByLabel, eight call sites, the canvas among them,
// filters containers.
func TestLabelsReachTheContainer(t *testing.T) {
	spec, _ := serviceSpec(ServiceSpec{Name: "web", Image: "nginx",
		Labels: map[string]string{LabelApp: "tile-1"}})
	if spec.TaskTemplate.ContainerSpec.Labels[LabelApp] != "tile-1" {
		t.Fatal("stackr.app is not on the container spec")
	}
	if spec.TaskTemplate.ContainerSpec.Labels[LabelManaged] != "true" {
		t.Fatal("stackr.managed is not on the container spec")
	}
}

// An absolute source is a host path, anything else a named volume. Get this
// backwards and a database's named volume becomes an empty host directory.
func TestParseMounts(t *testing.T) {
	got, err := parseMounts([]string{"stackr-data:/var/lib/pg", "/srv/media:/media:ro"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Type != mount.TypeVolume || got[0].Source != "stackr-data" {
		t.Fatalf("named volume: %+v", got[0])
	}
	if got[1].Type != mount.TypeBind || !got[1].ReadOnly {
		t.Fatalf("read-only bind: %+v", got[1])
	}
	share, err := parseMounts([]string{"stackr-stor-1:/tv:nocopy,ro"})
	if err != nil || !share[0].ReadOnly || share[0].VolumeOptions == nil || !share[0].VolumeOptions.NoCopy {
		t.Fatalf("storage mount options not read: %+v %v", share, err)
	}
	for _, rel := range []string{"./authelia:/config", "../x:/x"} {
		if _, err := parseMounts([]string{rel}); err == nil || !strings.Contains(err.Error(), "files:") {
			t.Fatalf("%s: want an error naming files:, got %v", rel, err)
		}
	}
	if _, err := parseMounts([]string{"nonsense"}); err == nil {
		t.Fatal("a line with no target must fail the deploy, not mount nothing")
	}
}

func TestServicePorts(t *testing.T) {
	got, err := servicePorts(map[string]string{"8080": "80/udp"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].PublishedPort != 8080 || got[0].TargetPort != 80 || got[0].Protocol != "udp" {
		t.Fatalf("port: %+v", got[0])
	}
	if got[0].PublishMode != swarm.PortConfigPublishModeIngress {
		t.Fatal("published ports must go through the routing mesh")
	}
	// Traefik needs the visitor's real IP, which the mesh SNATs away.
	host, err := servicePorts(map[string]string{"443": "443"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if host[0].PublishMode != swarm.PortConfigPublishModeHost {
		t.Fatal("host mode requested, mesh mode produced")
	}
}

func TestJobSpec(t *testing.T) {
	spec, err := jobSpec(ServiceSpec{
		Name:       "stkr_org_stack_env_tile_run-1a2b3c4d",
		Image:      "alpine",
		Entrypoint: []string{"sh"},
		Cmd:        []string{"-c", "echo hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Mode.ReplicatedJob == nil || spec.Mode.Replicated != nil {
		t.Fatalf("want replicated-job mode, got %+v", spec.Mode)
	}
	if *spec.Mode.ReplicatedJob.TotalCompletions != 1 {
		t.Errorf("want one completion")
	}
	if spec.TaskTemplate.RestartPolicy.Condition != swarm.RestartPolicyConditionNone {
		t.Errorf("a restarting one-shot is not a one-shot")
	}
	// Swarm refuses both on a job.
	if spec.UpdateConfig != nil || spec.EndpointSpec != nil {
		t.Errorf("update/endpoint config must be nil on a job")
	}
	// The command override has to replace the image's entrypoint, not be
	// handed to it as arguments.
	if got := spec.TaskTemplate.ContainerSpec.Command; len(got) != 1 || got[0] != "sh" {
		t.Errorf("entrypoint = %v", got)
	}
}

// Rootless buildkit is the one service that needs seccomp and apparmor off,
// and nothing else may get them by accident.
func TestUnconfinedIsExplicit(t *testing.T) {
	spec, err := serviceSpec(ServiceSpec{Name: "bk", Image: "moby/buildkit:rootless", Replicas: 1, Unconfined: true})
	if err != nil {
		t.Fatal(err)
	}
	p := spec.TaskTemplate.ContainerSpec.Privileges
	if p == nil || p.Seccomp.Mode != swarm.SeccompModeUnconfined || p.AppArmor.Mode != swarm.AppArmorModeDisabled {
		t.Fatalf("unconfined privileges: %+v", p)
	}
	plain, _ := serviceSpec(ServiceSpec{Name: "web", Image: "nginx", Replicas: 1})
	if plain.TaskTemplate.ContainerSpec.Privileges != nil {
		t.Fatal("a plain service got privileges")
	}
}
