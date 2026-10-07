package deploy

import "github.com/FyrmForge/stackr/internal/service/internal/store"

// PruneFiles is pruneFiles for the external tests.
func PruneFiles(f *Flow, t store.Tile, mounts []string) error { return f.pruneFiles(t, mounts) }

// SlotPath is slotPath for the external tests.
func SlotPath(tmp, n, rel string) (string, error) { return slotPath(tmp, n, rel) }
