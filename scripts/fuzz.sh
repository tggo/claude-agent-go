#!/usr/bin/env bash
# Run every native Go fuzz target for a short time. Usage: scripts/fuzz.sh [fuzztime]
# (default 10s). Targets are discovered with `go test -list`, so new Fuzz*
# functions are picked up automatically. Failing inputs are written by the go
# tool to <pkg>/testdata/fuzz/<Target>/ — commit them as regression seeds.
set -uo pipefail

fuzztime="${1:-10s}"
failed=0
ran=0

# `go test -list` prints each package's targets followed by "ok <pkg> <time>".
pending=()
while IFS= read -r line; do
  case "$line" in
    Fuzz*) pending+=("$line") ;;
    ok*)
      pkg=$(awk '{print $2}' <<<"$line")
      for target in "${pending[@]+"${pending[@]}"}"; do
        ran=$((ran + 1))
        echo "=== fuzz $pkg $target ($fuzztime)"
        if ! go test -run='^$' -fuzz="^${target}\$" -fuzztime="$fuzztime" "$pkg"; then
          failed=$((failed + 1))
        fi
      done
      pending=()
      ;;
  esac
done < <(go test -list '^Fuzz' ./... 2>/dev/null)

echo "fuzzed $ran target(s), $failed failed"
[ "$ran" -gt 0 ] && [ "$failed" -eq 0 ]
