#!/usr/bin/env bash
# batch-issues.sh — Create GitHub issues from a JSON file
# Usage: ./batch-issues.sh issues.json [--dry-run]
#
# JSON format: array of objects with title, body, labels (array of strings)
# Example:
# [
#   {"title": "Fix the thing", "body": "## What\n\nIt's broken", "labels": ["bug"]},
#   {"title": "Add the feature", "body": "## What\n\nNew stuff", "labels": ["feature", "later"]}
# ]

set -euo pipefail

REPO="AutumnsGrove/her-go"
MAX_ISSUES=50

# --- args ---
FILE="${1:-}"
DRY_RUN=false
if [[ "${2:-}" == "--dry-run" ]]; then DRY_RUN=true; fi

if [[ -z "$FILE" ]]; then
    echo "Usage: batch-issues.sh <issues.json> [--dry-run]"
    exit 1
fi

if [[ ! -f "$FILE" ]]; then
    echo "Error: file not found: $FILE"
    exit 1
fi

# --- validate ---
COUNT=$(jq length "$FILE")
if [[ "$COUNT" -eq 0 ]]; then
    echo "Error: no issues in file"
    exit 1
fi
if [[ "$COUNT" -gt "$MAX_ISSUES" ]]; then
    echo "Error: too many issues ($COUNT, max $MAX_ISSUES)"
    exit 1
fi

echo "=== Batch Issue Creator ==="
echo "File: $FILE"
echo "Repo: $REPO"
echo "Issues: $COUNT"
echo ""

# --- dry run: preview table ---
if $DRY_RUN; then
    echo "--- DRY RUN (no issues will be created) ---"
    echo ""
    printf "%-4s %-55s %s\n" "#" "Title" "Labels"
    printf "%-4s %-55s %s\n" "---" "-------" "------"
    for i in $(seq 0 $((COUNT - 1))); do
        TITLE=$(jq -r ".[$i].title" "$FILE")
        LABELS=$(jq -r ".[$i].labels | join(\", \")" "$FILE")
        printf "%-4s %-55s %s\n" "$((i + 1))" "${TITLE:0:55}" "$LABELS"
    done
    echo ""
    echo "Run without --dry-run to create these issues."
    exit 0
fi

# --- create issues ---
CREATED=0
FAILED=0

for i in $(seq 0 $((COUNT - 1))); do
    TITLE=$(jq -r ".[$i].title" "$FILE")
    BODY=$(jq -r ".[$i].body" "$FILE")

    # Build label args
    LABEL_ARGS=()
    LABEL_COUNT=$(jq -r ".[$i].labels | length" "$FILE")
    for j in $(seq 0 $((LABEL_COUNT - 1))); do
        LABEL=$(jq -r ".[$i].labels[$j]" "$FILE")
        LABEL_ARGS+=(--label "$LABEL")
    done

    # Create the issue
    if URL=$(gh issue create --repo "$REPO" --title "$TITLE" --body "$BODY" "${LABEL_ARGS[@]}" 2>&1); then
        printf "✓ %2d  %-50s  %s\n" "$((i + 1))" "${TITLE:0:50}" "$URL"
        CREATED=$((CREATED + 1))
    else
        printf "✗ %2d  %-50s  FAILED: %s\n" "$((i + 1))" "${TITLE:0:50}" "$URL"
        FAILED=$((FAILED + 1))
    fi
done

echo ""
echo "Done: $CREATED created, $FAILED failed"
