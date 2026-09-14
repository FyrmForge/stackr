package managedtiles

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// psql -At -F'|' output, plus the noise a real run can carry: a trailing blank
// line, and a row we can't read. One bad row must not lose the good ones.
func TestParseSliceStats(t *testing.T) {
	got := parseSliceStats("orders_api|4218|8413184\ncart_api|17|8265728\nbroken|notanumber|12\n\n")
	require.Len(t, got, 2, "parsed rows: %+v", got)
	assert.Equal(t, SliceStat{Xacts: 4218, Size: 8413184}, got["orders_api"])
	assert.NotContains(t, got, "broken", "unparseable row was kept")
}
