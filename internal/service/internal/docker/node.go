package docker

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

type nodeKey struct{}

// WithNode names the node a Docker call is for. The router picks that
// node's client by it, and the network allocators reserve from its range.
func WithNode(ctx context.Context, nodeID string) context.Context {
	return context.WithValue(ctx, nodeKey{}, nodeID)
}

// NodeFrom is the node id WithNode set; ok is false when none was.
func NodeFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(nodeKey{}).(string)
	return id, ok && id != ""
}

type pullOrgKey struct{}

// WithPullOrg marks a pull as made for an org (its slug), a deploy of one of
// its tiles: a cluster registry image it names must be that org's (or
// stackr's own), since the image ref is the org's to write.
func WithPullOrg(ctx context.Context, orgSlug string) context.Context {
	return context.WithValue(ctx, pullOrgKey{}, orgSlug)
}

// PullOrgFrom is the org slug WithPullOrg set.
func PullOrgFrom(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(pullOrgKey{}).(string)
	return s, ok && s != ""
}

// ErrNoNode is a Docker call made without a node in its context. Every
// caller decides which node it is for (see service/dockerrouter.go).
var ErrNoNode = errors.New("docker: call has no node in its context")

// Reserver hands out the next free block of a node's range (leaf/node).
type Reserver interface {
	Reserve(ctx context.Context, nodeID string, used []netip.Prefix) (netip.Prefix, error)
}

// Networker is the slice of the Docker client EnsureReserved needs.
type Networker interface {
	EnsureNetwork(ctx context.Context, name string, subnet netip.Prefix, labels map[string]string) error
	UsedSubnets(ctx context.Context) ([]netip.Prefix, error)
}

// subnetTries is how many blocks one create offers when concurrent creates
// race for them.
const subnetTries = 5

// EnsureReserved makes sure network name exists. When it has to be made, the
// block comes from the node's range in ctx, never from Docker: known are
// blocks the caller already holds (the node's env subnets), and commit, if
// set, is told the block before the network is made so a crash leaves it
// written down. A network that already exists returns the zero prefix. This
// is the one path of all three allocators (env, shared and ingress networks).
func EnsureReserved(
	ctx context.Context,
	n Networker,
	r Reserver,
	name string,
	labels map[string]string,
	known []netip.Prefix,
	commit func(netip.Prefix) error,
) (netip.Prefix, error) {
	err := n.EnsureNetwork(ctx, name, netip.Prefix{}, labels)
	if !errors.Is(err, ErrNeedsSubnet) {
		return netip.Prefix{}, err
	}
	node, ok := NodeFrom(ctx)
	if !ok {
		return netip.Prefix{}, ErrNoNode
	}
	if r == nil {
		return netip.Prefix{}, errors.New("docker: no subnet reserver wired")
	}
	var tried []netip.Prefix
	var taken error
	for range subnetTries {
		used, err := n.UsedSubnets(ctx)
		if err != nil {
			return netip.Prefix{}, err
		}
		used = append(append(used, known...), tried...)
		p, err := r.Reserve(ctx, node, used)
		if err != nil {
			return netip.Prefix{}, err
		}
		if commit != nil {
			if err := commit(p); err != nil {
				return netip.Prefix{}, err
			}
		}
		if err = n.EnsureNetwork(ctx, name, p, labels); !errors.Is(err, ErrSubnetTaken) {
			return p, err
		}
		taken = err
		tried = append(tried, p)
	}
	return netip.Prefix{}, taken
}

type logsSinceKey struct{}

// WithLogsSince makes a log read start at t (and take every line from there,
// whatever its tail). It rides in ctx so the Docker interface stays as it is.
func WithLogsSince(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, logsSinceKey{}, t)
}

// LogsSinceFrom is the time WithLogsSince set; ok is false when none was.
func LogsSinceFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(logsSinceKey{}).(time.Time)
	return t, ok
}
