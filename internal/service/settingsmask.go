package service

import "github.com/FyrmForge/stackr/internal/service/internal/store"

// MaskSettings hides protect_password in a settings blob the API returns; a
// blob written back with the mask keeps the stored password.
func MaskSettings(blob string) string { return store.MaskSettings(blob) }
