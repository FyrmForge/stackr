package store

import (
	"context"
	"time"
)

// User is a row of users. Role "admin" is the stackr admin.
type User struct {
	ID           string    `db:"id"`
	Email        string    `db:"email"`
	PasswordHash string    `db:"password_hash"`
	Name         string    `db:"name"`
	Role         string    `db:"role"`
	Active       bool      `db:"active"`
	AvatarPath   string    `db:"avatar_path"`
	Theme        string    `db:"theme"`
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}

type UserStore interface {
	Create(ctx context.Context, u User) error
	Get(ctx context.Context, id string) (User, error)
	GetByEmail(ctx context.Context, email string) (User, error)
	List(ctx context.Context) ([]User, error)
	Update(ctx context.Context, u User) error
	Delete(ctx context.Context, id string) error
}

var usersT = newTable[User]("users", nil)

type users struct{ crud[User] }

func (s users) GetByEmail(ctx context.Context, email string) (User, error) {
	return s.one(ctx, "email = ?", email)
}

func (s users) List(ctx context.Context) ([]User, error) { return s.many(ctx, "1 = 1") }
