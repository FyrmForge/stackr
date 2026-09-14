# Setup gate

Status: shipped and QA'd 2026-09-08.

An unfinished organization exposes only its setup flow: canvas, stack pages,
API operations and stray org mutations all honor the same gate. Middleware
derives the state once and the web and API route groups enforce it; five rig
rounds closed the escape paths.
