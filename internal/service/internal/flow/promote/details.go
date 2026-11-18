package promote

import (
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// createDetails is what a create row shows when opened: what the new tile
// will be, from the file's block and the row it makes, one "Label: value"
// per line. A line with no value is left out.
//
// vol, when set, words each volume spec of the block ("pgdata, new, 5120 MB").
func createDetails(tc TileConf, row store.Tile, vol func(spec string) string) string {
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
	specs := tc.Volumes
	if vol != nil {
		specs = nil
		for _, s := range tc.Volumes {
			specs = append(specs, vol(s))
		}
	}
	add("Volumes", strings.Join(specs, ", "))
	return strings.Join(out, "\n")
}
