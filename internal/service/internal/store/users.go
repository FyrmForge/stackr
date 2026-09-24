package store

import (
	"context"
	"time"
)

// User is a row of users. Role "admin" is the stackr admin.
type User struct {
	ID           string    `db:"id" json:"id"`
	Email        string    `db:"email" json:"email"`
	PasswordHash string    `db:"password_hash" json:"-"`
	Name         string    `db:"name" json:"name"`
	Role         string    `db:"role" json:"role"`
	Active       bool      `db:"active" json:"active"`
	AvatarPath   string    `db:"avatar_path" json:"avatar_path"`
	Theme        string    `db:"theme" json:"theme"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// Admin says the user is a stackr admin (owner in every org).
func (u User) Admin() bool { return u.Role == "admin" }

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
