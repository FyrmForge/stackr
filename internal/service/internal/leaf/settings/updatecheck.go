package settings

import "time"

// UpdateChecks are the update_check values, in the order a form shows them.
var UpdateChecks = []string{"off", "1h", "6h", "daily", "weekly"}

// UpdateCheckEvery reads an update_check value: how long after one check the
// next may run. ok is false for a value that is not one of UpdateChecks; "off"
// is ok with a zero interval.
func UpdateCheckEvery(raw string) (d time.Duration, ok bool) {
	switch raw {
	case "off":
		return 0, true
	case "1h":
		return time.Hour, true
	case "6h":
		return 6 * time.Hour, true
	case "daily":
		return 24 * time.Hour, true
	case "weekly":
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}
