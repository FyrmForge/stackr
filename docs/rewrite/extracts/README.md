# Extracts

One file per row of REWRITE.md "What comes over from the old code", written
by narrow sub-agents that read only that row's files at commit `c2423f0`.
The builder reads these, never the old code.

Header, five lines: Source, Commit, Taken, Cut, Cuts belong to. Body: the
kept code (or, for reference-only rows, a prose description of what the
code decides), every cut marked `// extract: dropped X, belongs in leaf/Y`.
Last line: source lines vs extract lines; an extract bigger than its source
was copied, not filtered, and gets redone.

Filters, in order: the row's take/leave columns; the layering rules (no
store call, no Docker call outside the wrapper, no auth check, no status
decision inside the kept code); size.

Skim status: see `docs/rewrite/PROGRESS.md`.
