package store

import (
	"context"
	"time"
)

// SecurityEvent is a row of security_events: one audited act. The table is
// append only (triggers refuse update and delete), so there is no Update or
// Delete here.
type SecurityEvent struct {
	ID         string    `db:"id" json:"id"`
	At         time.Time `db:"at" json:"at"`
	Action     string    `db:"action" json:"action"`
	ActorID    string    `db:"actor_id" json:"actor_id"`
	KeyID      string    `db:"key_id" json:"key_id"`
	TargetKind string    `db:"target_kind" json:"target_kind"`
	TargetID   string    `db:"target_id" json:"target_id"`
	IP         string    `db:"ip" json:"ip"`
	UserAgent  string    `db:"user_agent" json:"user_agent"`
	Detail     string    `db:"detail" json:"detail"`
}

// SecurityFilter narrows a listing; a zero field matches everything.
type SecurityFilter struct {
	Action     string
	ActorID    string
	TargetKind string
	TargetID   string
	Since      time.Time // At >= Since
	Until      time.Time // At < Until
	Limit      int       // 0 = every match
}

type SecurityEventStore interface {
	Create(ctx context.Context, e SecurityEvent) error
	// Find is newest first.
	Find(ctx context.Context, f SecurityFilter) ([]SecurityEvent, error)
}

var securityEventsT = newTable[SecurityEvent]("security_events", nil)

type securityEvents struct{ crud[SecurityEvent] }

func (s securityEvents) Find(ctx context.Context, f SecurityFilter) ([]SecurityEvent, error) {
	where, args := "1 = 1", []any{}
	add := func(cond string, v any) { where, args = where+" AND "+cond, append(args, v) }
	if f.Action != "" {
		add("action = ?", f.Action)
	}
	if f.ActorID != "" {
		add("actor_id = ?", f.ActorID)
	}
	if f.TargetKind != "" {
		add("target_kind = ?", f.TargetKind)
	}
	if f.TargetID != "" {
		add("target_id = ?", f.TargetID)
	}
	if !f.Since.IsZero() {
		add("at >= ?", f.Since.UTC())
	}
	if !f.Until.IsZero() {
		add("at < ?", f.Until.UTC())
	}
	where += " ORDER BY at DESC, id DESC"
	if f.Limit > 0 {
		where += " LIMIT ?"
		args = append(args, f.Limit)
	}
	return s.many(ctx, where, args...)
}
