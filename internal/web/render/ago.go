package render

import (
	"fmt"
	"time"
)

// Ago reads "just now", "5 min ago", "3 h ago", "2 days ago"; zero = never.
func Ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "not seen yet"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}
