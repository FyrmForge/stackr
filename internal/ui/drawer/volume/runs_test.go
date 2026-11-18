package volume

import "testing"

func TestRunsAndCronOf(t *testing.T) {
	for cron, want := range map[string]string{
		"0 3 * * *": "Every day at 03:00", "0 4 * * 0": "Every Sunday at 04:00", "@daily": "Every day at 00:00",
		"5 * * * *": "Every hour at :05", "*/5 * * * *": "*/5 * * * *", "0 3 * * 1": "0 3 * * 1",
	} {
		if got := Runs(cron); got != want {
			t.Errorf("Runs(%q) = %q, want %q", cron, got, want)
		}
	}
	for _, c := range []struct{ repeat, at, want string }{
		{"day", "", "0 3 * * *"}, {"week", "04:30", "30 4 * * 0"}, {"hour", "00:15", "15 * * * *"},
	} {
		if got, err := CronOf(c.repeat, c.at); err != nil || got != c.want {
			t.Errorf("CronOf(%q, %q) = %q, %v, want %q", c.repeat, c.at, got, err, c.want)
		}
		if Runs(c.want) == c.want {
			t.Errorf("%q is not put in words", c.want)
		}
	}
	if _, err := CronOf("month", ""); err == nil {
		t.Error("an unknown repeat passed")
	}
	if _, err := CronOf("day", "25:00"); err == nil {
		t.Error("a bad time passed")
	}
}
