# Declarative `moved:` renames

Status: shipped 2026-09-13.

Top-level `moved:` entries provide explicit, one-hop renames for tiles and
environments in stack files and stacks and shared instances in org files.
Planning refuses ambiguous or destructive mappings; apply re-slugs the live
row so attached data survives. See `stackconf/moved.go` and `orgconf/moved.go`.
