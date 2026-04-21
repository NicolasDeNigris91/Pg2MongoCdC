#!/usr/bin/env bash
# Run every chaos scenario and fail loudly on the first non-zero exit.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
mapfile -t SCENARIOS < <(find "$SCRIPT_DIR/scenarios" -maxdepth 1 -type f -name '*.sh' | sort)

# Every scenario MUST declare its PASS criterion as a comment.
echo "Pre-flight: checking every scenario has a PASS: comment ..."
for s in "${SCENARIOS[@]}"; do
  if ! grep -q '^# PASS:' "$s"; then
    echo "FAIL: $s has no 'PASS:' comment" >&2
    exit 2
  fi
done

PASSED=0
FAILED=0
FAILED_LIST=()

for s in "${SCENARIOS[@]}"; do
  echo ""
  echo "###################################################"
  echo "# $(basename "$s")"
  echo "###################################################"
  if bash "$s"; then
    PASSED=$((PASSED + 1))
  else
    FAILED=$((FAILED + 1))
    FAILED_LIST+=("$(basename "$s")")
  fi
done

echo ""
echo "==================================================="
echo "Chaos summary: PASSED=$PASSED FAILED=$FAILED"
if [ $FAILED -gt 0 ]; then
  printf '  failed: %s\n' "${FAILED_LIST[@]}"
  exit 1
fi
echo "All scenarios passed."
