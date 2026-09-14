# Config edit policy and UI fixes

Status: shipped and rig-verified 2026-09-06.

This batch fixed live run rows, log rendering, inherited domains and graph UI,
then added `ui_edits: block | stage` and declared domain resources. Managed
write holes found during QA were gated the same day. The work spans
`stackconf`, `orgconf`, project/app handlers and shared web components.
