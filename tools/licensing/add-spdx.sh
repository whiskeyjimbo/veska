#!/usr/bin/env bash
# Idempotently prepend SPDX headers to every .go file in the repo.
# SPDX lines are line comments placed at the very top, separated from the rest
# of the file by a blank line. This is valid even for files whose first line is
# a //go:build constraint (line comments + blank lines are permitted before it)
# and keeps any package doc comment attached to `package` (the blank line stops
# the SPDX block from being absorbed as the doc comment).
set -euo pipefail

read -r -d '' HEADER <<'EOF' || true
// SPDX-License-Identifier: AGPL-3.0-only
EOF

stamped=0
skipped=0
while IFS= read -r f; do
	if head -5 "$f" | grep -q "SPDX-License-Identifier"; then
		skipped=$((skipped + 1))
		continue
	fi
	tmp="$f.spdxtmp"
	printf '%s\n\n' "$HEADER" >"$tmp"
	cat "$f" >>"$tmp"
	mv "$tmp" "$f"
	stamped=$((stamped + 1))
done < <(find . -name '*.go' -not -path './.git/*')

echo "stamped=$stamped skipped=$skipped"
