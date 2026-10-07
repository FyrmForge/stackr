package store

import (
	"context"
	"time"
)

// Route is a row of routes: a host that goes to an address outside stackr.
type Route struct {
	ID        string    `db:"id" json:"id"`
	Host      string    `db:"host" json:"host"`
	Mode      string    `db:"mode" json:"mode"` // passthrough | http | https
	Target    string    `db:"target" json:"target"`
	Insecure  bool      `db:"insecure" json:"insecure"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type RouteStore interface {
	Create(ctx context.Context, r Route) error
	Get(ctx context.Context, id string) (Route, error)
	List(ctx context.Context) ([]Route, error)
	Update(ctx context.Context, r Route) error
	Delete(ctx context.Context, id string) error
}

var routesT = newTable[Route]("routes", nil)

type routes struct{ crud[Route] }

func (s routes) List(ctx context.Context) ([]Route, error) {
	return s.many(ctx, "1 = 1 ORDER BY host")
}
