#!/usr/bin/env bash
# Count lines per file type, skipping generated files. Tests and vendored files get their own tables.
set -euo pipefail

cd "$(dirname -- "${BASH_SOURCE[0]}")/.."

vendored='/vendor/|\.min\.(js|css)$|^ui/static/css/xterm\.css$'
tests='_test\.go$|/testdata/'

git ls-files \
  | grep -vE '_templ\.go$|^ui/static/css/output\.css$|^docs/openapi\.json$|^go\.sum$|^ui/static/js/elements/' \
  | while IFS= read -r f; do
      [[ -f $f ]] || continue
      # Go convention: "// Code generated ... DO NOT EDIT." near the top (sqlc, staticmanifest, ...)
      head -n 5 -- "$f" | grep -qE '^// Code generated .* DO NOT EDIT' && continue
      grep -Iq . -- "$f" || continue # skip binaries and empty files
      ext=${f##*/}; [[ ${ext:1} == *.* ]] && ext=.${ext##*.} # no extension: show the file name
      group=own
      [[ $f =~ $tests ]] && group=tests
      [[ $f =~ $vendored ]] && group=vendored
      printf '%s\t%s\t%s\n' "$group" "$ext" "$(wc -l < "$f")"
    done \
  | awk -F'\t' '
      { lines[$1, $2] += $3; files[$1, $2]++; total[$1] += $3; exts[$1] = exts[$1] " " $2 }
      function table(g,   e, seen, list, n, i, cmd) {
        if (!(g in total)) return
        printf "\n== %s ==\n", g
        cmd = "sort -k4 -nr"
        n = split(exts[g], list, " ")
        for (i = 1; i <= n; i++) {
          e = list[i]; if (e in seen) continue; seen[e] = 1
          printf "%-14s %6d files %9d lines\n", e, files[g, e], lines[g, e] | cmd
        }
        close(cmd)
        printf "%-14s %6s       %9d lines\n", "total", "", total[g]
      }
      END { table("own"); table("tests"); table("vendored") }'
