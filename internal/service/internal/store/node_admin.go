package store

import (
	"context"
	"time"
)

// ForgottenNode is a node that was forgotten: its envs still carry its id.
type ForgottenNode struct {
	ID          string    `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	ForgottenAt time.Time `db:"forgotten_at" json:"forgotten_at"`
}

type ForgottenNodeStore interface {
	Create(ctx context.Context, f ForgottenNode) error
	List(ctx context.Context) ([]ForgottenNode, error)
}

type forgottenNodes struct{ q querier }

func (s forgottenNodes) Create(ctx context.Context, f ForgottenNode) error {
	_, err := s.q.NamedExecContext(ctx, `INSERT INTO forgotten_nodes (id, name, forgotten_at) VALUES (:id, :name, :forgotten_at)`, f)
	return mapErr(err)
}

func (s forgottenNodes) List(ctx context.Context) ([]ForgottenNode, error) {
	var rows []ForgottenNode
	err := s.q.SelectContext(ctx, &rows, `SELECT id, name, forgotten_at FROM forgotten_nodes ORDER BY forgotten_at`)
	return rows, mapErr(err)
}
