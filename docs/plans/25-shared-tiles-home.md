# Shared tiles home

Status: shipped 2026-09-07.

Stack-scoped tiles no longer live accidentally in the first user environment.
Each stack has a hidden home environment which owns shared runtime rows while
visible environments consume them. Creation, planning and graph filtering are
implemented in the repo, `stackconf` and web graph packages.
