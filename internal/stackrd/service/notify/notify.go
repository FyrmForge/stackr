// Package notify pushes "something changed" events to browsers over the hamr
// websocket hub, and records events in the in-app notification center.
// Websocket events carry no data, clients react by re-fetching the authed
// HTTP endpoints they already render from, so the socket never needs its own
// auth story beyond the session.
package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/hamr/pkg/websocket"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Room names browsers join (see frontend/static/js/live.js).
const (
	RoomContainers    = "containers"
	RoomServer        = "server:local"
	RoomNotifications = "notifications"
	// RoomFlows carries the traffic sampler's tick. It is global rather than
	// per-stack: the sampler reads conntrack for the whole host and would have
	// to look up every tile's stack to fan out. Canvases showing traffic join
	// it and re-fetch their own status endpoint.
	RoomFlows = "flows"
)

// Notification kinds; each is gated by a "notify.<kind>" settings row.
const (
	KindDeployFailed = "deploy_failed"
	KindDeployDone   = "deploy_done"
	KindCronFailed   = "cron_failed"
	KindImageUpdate  = "image_update"
	// KindImageCheckFailed is separate from KindImageUpdate on purpose. They
	// shared a kind, so anyone who turned "New image version" off also stopped
	// hearing that the check itself was broken — a notification system must not
	// let an opt-out silence an alert.
	KindImageCheckFailed = "image_check_failed"
)

// Kinds lists every kind with its label and default state (for settings UI
// and gating). deploy_done is opt-in noise; failures default on.
var Kinds = []struct {
	Kind      string
	Label     string
	DefaultOn bool
}{
	{KindDeployFailed, "Deployment failed", true},
	{KindCronFailed, "Cron run failed", true},
	{KindDeployDone, "Deployment succeeded", false},
	{KindImageUpdate, "New image version", true},
	{KindImageCheckFailed, "Image check failed", true},
}

// DeployFailed is the one title and body for a deploy that will not happen,
// however it failed. The engine said "Deploy failed: x" and the CI gate said
// "Deploy blocked: x" for what is one event to the person reading it.
func DeployFailed(tileName, reason string) (title, body string) {
	return "Deploy failed: " + tileName, reason
}

func ProjectRoom(projectID string) string { return "project:" + projectID }

// OrgRoom carries org-canvas changes (layout today).
func OrgRoom(orgID string) string { return "org:" + orgID }

type Notifier struct {
	em    *websocket.Emitter
	store repo.Store
}

func New(hub *websocket.Hub, store repo.Store) *Notifier {
	return &Notifier{em: websocket.NewEmitter(hub), store: store}
}

// defaultOn is what a kind does for someone who has never changed it.
func defaultOn(kind string) bool {
	for _, k := range Kinds {
		if k.Kind == kind {
			return k.DefaultOn
		}
	}
	return false
}

// EnabledForUser reports whether one person wants a kind.
//
// There is deliberately no instance-wide setting: who gets told about a failed
// deploy is a personal choice, and a server-level default only created the
// question of whether an admin's switch outranks yours. An untouched account
// falls back to the kind's built-in default (failures on, successes off).
func EnabledForUser(u *repo.User, kind string) bool {
	if u != nil && u.NotifyPrefs != "" {
		var prefs map[string]bool
		if err := json.Unmarshal([]byte(u.NotifyPrefs), &prefs); err == nil {
			if want, ok := prefs[kind]; ok {
				return want
			}
		}
	}
	return defaultOn(kind)
}

// Push fans an event out to every active user who wants this kind, one row
// each, then nudges open notification centers. Old entries are pruned past 30
// days.
//
// Callers are background workers (deploy engine, cron runner) with no current
// user, so recipients are enumerated from the store rather than taken from a
// request.
func (n *Notifier) Push(ctx context.Context, kind, title, body, link string) {
	if n == nil {
		return
	}
	users, err := n.store.ListUsers(ctx)
	if err != nil {
		return
	}
	sent := false
	for i := range users {
		u := &users[i]
		if !u.Active || !EnabledForUser(u, kind) {
			continue
		}
		if err := n.store.CreateNotification(ctx, &repo.Notification{
			ID:        uuid.New().String(),
			UserID:    u.ID,
			Kind:      kind,
			Title:     title,
			Body:      body,
			Link:      link,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			slog.Error("notification not saved", "user", u.ID, "kind", kind, "error", err)
			continue
		}
		sent = true
	}
	if !sent {
		return
	}
	_ = n.store.PruneNotifications(ctx, time.Now().Add(-30*24*time.Hour))
	n.send(RoomNotifications, "notification")
}

func (n *Notifier) send(room, kind string) {
	if n == nil {
		return // wiring optional in tests
	}
	n.em.ToRoom(room, websocket.NewEvent(websocket.EventType(kind), nil))
}

// Project signals that something in a stack changed (statuses, runs, deploys).
func (n *Notifier) Project(projectID string) { n.send(ProjectRoom(projectID), "project") }

// Org signals that something on the org canvas changed.
func (n *Notifier) Org(orgID string) { n.send(OrgRoom(orgID), "org") }

// Containers signals container state changes.
func (n *Notifier) Containers() { n.send(RoomContainers, "containers") }

// Server signals fresh host samples / daemon state.
func (n *Notifier) Server() { n.send(RoomServer, "server") }

// Flows signals a fresh inter-tile traffic sample.
func (n *Notifier) Flows() { n.send(RoomFlows, "flows") }

// AccessChanged tells one person's open pages that their standing changed.
//
// It exists because a websocket room is checked when it is joined and never
// again (cmd/stackrd/ws.go), so a socket outlives the membership that let it
// in. The client answers by leaving and re-joining every room it declares,
// which runs the join check again and drops the rooms it may no longer read.
//
// ponytail: this trusts the client to re-join. A socket that ignores the
// event keeps its rooms until it disconnects, and what that leaks is event
// kinds with no payload — that something in an org changed, never what.
// Closing it from the server needs a Disconnect(subjectID) on hamr's Hub,
// which does not exist in v0.35.0.
func (n *Notifier) AccessChanged(userID string) {
	if n == nil || userID == "" {
		return
	}
	n.em.ToSubject(userID, websocket.NewEvent("access", nil))
}
