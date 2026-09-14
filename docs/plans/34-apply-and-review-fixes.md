# Apply safety and review fixes

Status: phases 0–5 shipped and rig-verified 2026-09-12.

Apply became durable and convergent, plans became one-per-commit, destructive
paths gained guards, promotion stopped bypassing pending plans, and the review
findings around staging, waiting and deletion were closed. The central rule is
that HTTP enqueues work and `stackconf` owns planning and apply state.
