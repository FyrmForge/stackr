package components

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A "two significant figures for tiny values" branch printed an idle
// container's CPU as "2.5e-05%" on the tile Metrics tab. MetricChart is shared
// by ping, CPU, memory, disk and both network series, so a readout must never
// carry an exponent however small the value gets.
func TestFmtValNeverGoesScientific(t *testing.T) {
	for _, v := range []float64{0, 1e-9, 2.5e-05, 0.004, 0.049, 0.05, 0.3, 12.5, 99.94, 444.2, 1e9} {
		got := fmtVal(v)
		require.NotContains(t, got, "e", "fmtVal(%v) = %q", v, got)
		require.False(t, strings.ContainsAny(got, "E+"), "fmtVal(%v) = %q", v, got)
	}
}

func TestFmtValReadouts(t *testing.T) {
	require.Equal(t, "0.0", fmtVal(0))
	require.Equal(t, "0.0", fmtVal(2.5e-05), "four orders below the axis is zero to a reader")
	require.Equal(t, "0.4", fmtVal(0.43))
	require.Equal(t, "12.5", fmtVal(12.5))
	require.Equal(t, "99.9", fmtVal(99.94))
	require.Equal(t, "444", fmtVal(444.2))
}
