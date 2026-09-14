# Empty organization wizard

Status: shipped 2026-09-08.

Organization creation now makes an empty org first; the wizard can fill it
from a repository config or by hand and branches without creating speculative
stack state. Setup handlers live in the settings package and config ingestion
is owned by `orgconf` and `stackconf`.
