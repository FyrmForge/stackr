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
	ID           string     `db:"id"`
	Kind         string     `db:"kind"`
	State        string     `db:"state"`
	ReleaseID    *string    `db:"release_id"`
	LockSet      StringList `db:"lock_set"`
	Payload      string     `db:"payload"`
	WaitingParam *string    `db:"waiting_param"`
	LogPath      string     `db:"log_path"`
	Error        string     `db:"error"`
	CreatedAt    time.Time  `db:"created_at"`
	StartedAt    *time.Time `db:"started_at"`
	FinishedAt   *time.Time `db:"finished_at"`
}

type JobStore interface {
	Create(ctx context.Context, j Job) error
	Get(ctx context.Context, id string) (Job, error)
	ListByState(ctx context.Context, states ...string) ([]Job, error)
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
	var b []byte
	switch v := src.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("StringList: cannot scan %T", src)
	}
	return json.Unmarshal(b, (*[]string)(l))
}
