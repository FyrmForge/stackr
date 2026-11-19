package volume

import (
	"fmt"
	"strings"
	"time"
)

// Runs is a cron line as the Backups tab says it. Only the presets the add
// form makes are put in words; any other line is shown as it is.
func Runs(cron string) string {
	switch cron {
	case "@hourly":
		cron = "0 * * * *"
	case "@daily", "@midnight":
		cron = "0 0 * * *"
	case "@weekly":
		cron = "0 0 * * 0"
	}
	f := strings.Fields(cron)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" {
		return cron
	}
	m, h := f[0], f[1]
	if h == "*" && f[4] == "*" && number(m) {
		return fmt.Sprintf("Every hour at :%02s", m)
	}
	if !number(m) || !number(h) {
		return cron
	}
	at := fmt.Sprintf("%02s:%02s", h, m)
	switch f[4] {
	case "*":
		return "Every day at " + at
	case "0":
		return "Every Sunday at " + at
	}
	return cron
}

// CronOf is the cron line of the add form's Repeat (day, week, hour) and At
// ("15:04"; empty is 03:00 and, for an hour, the minute 0).
func CronOf(repeat, at string) (string, error) {
	if at == "" {
		at = "03:00"
	}
	t, err := time.Parse("15:04", at)
	if err != nil {
		return "", fmt.Errorf("at is a time like 03:00")
	}
	switch repeat {
	case "day":
		return fmt.Sprintf("%d %d * * *", t.Minute(), t.Hour()), nil
	case "week":
		return fmt.Sprintf("%d %d * * 0", t.Minute(), t.Hour()), nil
	case "hour":
		return fmt.Sprintf("%d * * * *", t.Minute()), nil
	}
	return "", fmt.Errorf("repeat is day, week or hour")
}

func number(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
