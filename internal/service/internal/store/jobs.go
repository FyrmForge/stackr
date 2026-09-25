package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Job is a row of jobs.
type Job struct {
	ID           string     `db:"id" json:"id"`
	Kind         string     `db:"kind" json:"kind"`
	State        string     `db:"state" json:"state"`
	ReleaseID    *string    `db:"release_id" json:"release_id"`
	LockSet      StringList `db:"lock_set" json:"lock_set"`
	Payload      string     `db:"payload" json:"payload"`
	WaitingParam *string    `db:"waiting_param" json:"waiting_param"`
	LogPath      string     `db:"log_path" json:"-"`
	Error        string     `db:"error" json:"error"`
	CreatedAt    time.Time  `db:"created_at" json:"created_at"`
	StartedAt    *time.Time `db:"started_at" json:"started_at"`
	FinishedAt   *time.Time `db:"finished_at" json:"finished_at"`
}

type JobStore interface {
	Create(ctx context.Context, j Job) error
	Get(ctx context.Context, id string) (Job, error)
	ListByState(ctx context.Context, states ...string) ([]Job, error)
	// ListTouching is the newest jobs whose lock set holds any of tileIDs,
	// optionally of one kind ("" = any), newest first, at most limit.
	ListTouching(ctx context.Context, tileIDs []string, kind string, limit int) ([]Job, error)
	Update(ctx context.Context, j Job) error
	Delete(ctx context.Context, id string) error
}

var jobsT = newTable[Job]("jobs", nil)

type jobs struct{ crud[Job] }

func (s jobs) ListByState(ctx context.Context, states ...string) ([]Job, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = st
	}
	return s.many(ctx, "state IN (?"+strings.Repeat(", ?", len(states)-1)+")", args...)
}

func (s jobs) ListTouching(ctx context.Context, tileIDs []string, kind string, limit int) ([]Job, error) {
	if len(tileIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(tileIDs)+2)
	for _, id := range tileIDs {
		args = append(args, id)
	}
	where := "EXISTS (SELECT 1 FROM json_each(lock_set) WHERE value IN (?" + strings.Repeat(", ?", len(tileIDs)-1) + "))"
	if kind != "" {
		where += " AND kind = ?"
		args = append(args, kind)
	}
	return s.many(ctx, where+" ORDER BY created_at DESC LIMIT ?", append(args, limit)...)
}

// StringList is a JSON array column.
type StringList []string

func (l StringList) Value() (driver.Value, error) {
	if l == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]string(l))
	return string(b), err
}

func (l *StringList) Scan(src any) error {
	return scanJSON("StringList", src, (*[]string)(l))
}

// StringMap is a JSON object column.
type StringMap map[string]string

func (m StringMap) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]string(m))
	return string(b), err
}

func (m *StringMap) Scan(src any) error {
	return scanJSON("StringMap", src, (*map[string]string)(m))
}

func scanJSON(name string, src, dst any) error {
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("%s: cannot scan %T", name, src)
	}
	return json.Unmarshal(b, dst)
}
