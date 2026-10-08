package jobs

// BeforePark sets a hook that runs after a handler returns and before its job parks.
func (r *Runner) BeforePark(f func()) { r.beforePark = f }
