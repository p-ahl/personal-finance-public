#!/usr/bin/env bash
# Run all implemented downloaders for today.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if [[ -x "$ROOT/bin/finance" ]]; then
  FINANCE="$ROOT/bin/finance"
else
  FINANCE=(go run ./cmd/finance)
fi

echo "== kaspa =="
"${FINANCE[@]}" download kaspa
