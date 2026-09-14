package repo

import "time"

// User represents a user account.
type User struct {
	ID           string `db:"id"`
	Email        string `db:"email"`
	PasswordHash string `db:"password_hash"`
	Name         string `db:"name"`
	Role         string `db:"role"`
	Active       bool   `db:"active"`
	// NotifyPrefs overrides the instance notification defaults as a JSON
	// object of {kind: bool}. Empty means "inherit", so raising a default
	// reaches everyone who never expressed a preference.
	NotifyPrefs string `db:"notify_prefs"`
	// GraphPrefs holds the canvas display settings as JSON, mirroring how
	// NotifyPrefs works. Empty means "client defaults".
	GraphPrefs string `db:"graph_prefs"`
	// Theme is light, dark or system. system follows the OS setting.
	Theme string `db:"theme"`
	// AvatarPath is where the uploaded image lives in FileStorage, "" = none
	// (surfaces fall back to initials).
	AvatarPath string    `db:"avatar_path"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}
