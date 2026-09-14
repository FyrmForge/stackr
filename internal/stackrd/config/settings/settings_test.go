package settings

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptrI(v int) *int         { return &v }
func ptrF(v float64) *float64 { return &v }

// A settings form only ever shows a subset of the cascade fields; saving one
// must not wipe the fields it never rendered.
func TestMergeKeepsUnsubmittedFields(t *testing.T) {
	existing := Settings{
		CronTimeoutMin:       ptrI(45),
		RunRetentionDays:     ptrI(90),
		MetricRetentionHours: ptrI(48),
	}
	// The stack form posts only the three cascade knobs.
	got := Merge(existing, url.Values{
		"cron_timeout_min": {"60"},
		"cpu_limit":        {"2"},
		"mem_limit_mb":     {"512"},
	})

	if assert.NotNil(t, got.CronTimeoutMin, "cron timeout: want 60") {
		assert.Equal(t, 60, *got.CronTimeoutMin, "cron timeout: want 60")
	}
	if assert.NotNil(t, got.RunRetentionDays, "run retention wiped") {
		assert.Equal(t, 90, *got.RunRetentionDays, "run retention wiped")
	}
	if assert.NotNil(t, got.MetricRetentionHours, "metric retention wiped") {
		assert.Equal(t, 48, *got.MetricRetentionHours, "metric retention wiped")
	}
}

func TestMergeFieldSemantics(t *testing.T) {
	tests := []struct {
		name     string
		existing Settings
		vals     url.Values
		check    func(*testing.T, Settings)
	}{
		{
			name:     "empty value clears to inherit",
			existing: Settings{CPULimit: ptrF(4)},
			vals:     url.Values{"cpu_limit": {""}},
			check: func(t *testing.T, s Settings) {
				assert.Nil(t, s.CPULimit)
			},
		},
		{
			name:     "explicit zero limit is a real override",
			existing: Settings{CPULimit: ptrF(4), MemLimitMB: ptrI(512)},
			vals:     url.Values{"cpu_limit": {"0"}, "mem_limit_mb": {"0"}},
			check: func(t *testing.T, s Settings) {
				if assert.NotNil(t, s.CPULimit, "cpu: want explicit 0") {
					assert.Equal(t, float64(0), *s.CPULimit, "cpu: want explicit 0")
				}
				if assert.NotNil(t, s.MemLimitMB, "mem: want explicit 0") {
					assert.Equal(t, 0, *s.MemLimitMB, "mem: want explicit 0")
				}
			},
		},
		{
			name:     "zero timeout clears instead (no meaningful zero)",
			existing: Settings{CronTimeoutMin: ptrI(30)},
			vals:     url.Values{"cron_timeout_min": {"0"}},
			check: func(t *testing.T, s Settings) {
				assert.Nil(t, s.CronTimeoutMin)
			},
		},
		{
			name:     "garbage clears rather than corrupting",
			existing: Settings{MemLimitMB: ptrI(256)},
			vals:     url.Values{"mem_limit_mb": {"abc"}},
			check: func(t *testing.T, s Settings) {
				assert.Nil(t, s.MemLimitMB)
			},
		},
		{
			name:     "negative clears",
			existing: Settings{CPULimit: ptrF(2)},
			vals:     url.Values{"cpu_limit": {"-1"}},
			check: func(t *testing.T, s Settings) {
				assert.Nil(t, s.CPULimit)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, Merge(tc.existing, tc.vals))
		})
	}
}

// An explicit 0 at a lower level must beat a non-zero limit set above it,
// that is the only way to say "unlimited here" once a parent sets a cap.
func TestResolveExplicitZeroOverridesParent(t *testing.T) {
	server := Settings{CPULimit: ptrF(2), MemLimitMB: ptrI(1024)}
	stack := Settings{CPULimit: ptrF(0), MemLimitMB: ptrI(0)}

	r := Resolve(server, stack)
	assert.Equal(t, float64(0), r.CPULimit, "cpu: want 0 (unlimited)")
	assert.Equal(t, 0, r.MemLimitMB, "mem: want 0 (unlimited)")
}

func TestResolveCascadeOrder(t *testing.T) {
	server := Settings{CronTimeoutMin: ptrI(10), CPULimit: ptrF(1)}
	stack := Settings{CronTimeoutMin: ptrI(20)}
	env := Settings{CPULimit: ptrF(4)}

	r := Resolve(server, stack, env)
	assert.Equal(t, 20, r.CronTimeoutMin, "stack should win on timeout")
	assert.Equal(t, float64(4), r.CPULimit, "env should win on cpu")
	assert.Equal(t, builtin.RunRetentionDays, r.RunRetentionDays, "unset field should fall back to builtin")
}

// Round-tripping through storage must preserve an explicit zero, omitempty on
// a pointer field keeps it, a plain value field would silently drop it.
func TestExplicitZeroSurvivesJSONRoundTrip(t *testing.T) {
	s := Settings{CPULimit: ptrF(0)}
	got := Parse(s.JSON())
	require.NotNil(t, got.CPULimit, "explicit zero lost in round trip")
	require.Equal(t, float64(0), *got.CPULimit, "explicit zero lost in round trip")
}

// An unchecked checkbox submits nothing, so the form pairs it with a hidden
// "0" under the same name. Merge must see the checkbox's value when it is
// ticked and the hidden zero when it is not, reading the first value instead
// of the last made the setting one-way: on, and never off again.
func TestMergeCheckboxTogglesBothWays(t *testing.T) {
	on := Merge(Settings{}, url.Values{
		"protect_auto_domains": {"0", "1"}, // hidden, then the ticked box
	})
	require.NotNil(t, on.ProtectAutoDomains, "ticked box did not turn it on: %+v", on.ProtectAutoDomains)
	require.True(t, *on.ProtectAutoDomains, "ticked box did not turn it on: %+v", on.ProtectAutoDomains)
	off := Merge(on, url.Values{
		"protect_auto_domains": {"0"}, // hidden alone: the box was unticked
	})
	require.NotNil(t, off.ProtectAutoDomains, "unticked box did not turn it off: %+v", off.ProtectAutoDomains)
	require.False(t, *off.ProtectAutoDomains, "unticked box did not turn it off: %+v", off.ProtectAutoDomains)
	// A form that never mentions the key leaves the override alone.
	untouched := Merge(on, url.Values{"cpu_limit": {"1"}})
	if assert.NotNil(t, untouched.ProtectAutoDomains, "an unrelated form wiped the setting") {
		assert.True(t, *untouched.ProtectAutoDomains, "an unrelated form wiped the setting")
	}
}

// build_node is instance-wide: submitting it empty means "the manager", not
// "inherit", and it survives a save of a form that never showed it.
func TestBuildNodeMerge(t *testing.T) {
	s := Merge(Settings{}, url.Values{"build_node": {"node-2"}})
	require.NotNil(t, s.BuildNode)
	assert.Equal(t, "node-2", Resolve(s).BuildNode)
	s = Merge(s, url.Values{"cpu_limit": {"1"}})
	assert.Equal(t, "node-2", Resolve(s).BuildNode, "untouched by a form without the field")
	s = Merge(s, url.Values{"build_node": {""}})
	assert.Equal(t, "", Resolve(s).BuildNode)
}
