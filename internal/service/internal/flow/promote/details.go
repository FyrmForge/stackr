package promote

import (
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// createDetails is what a create row shows when opened: what the new tile
// will be, from the file's block and the row it makes, one "Label: value"
// per line. A line with no value is left out.
func createDetails(tc TileConf, row store.Tile) string {
	var out []string
	add := func(label, v string) {
		if v != "" {
			out = append(out, label+": "+v)
		}
	}
	add("Engine", tc.Engine)
	if tc.Major > 0 {
		add("Major", strconv.Itoa(tc.Major))
	}
	add("Image", row.ImageRef)
	add("Command", tc.Command)
	if tc.Replicas > 1 {
		add("Replicas", strconv.Itoa(tc.Replicas))
	}
	add("Volumes", strings.Join(tc.Volumes, ", "))
	return strings.Join(out, "\n")
}
