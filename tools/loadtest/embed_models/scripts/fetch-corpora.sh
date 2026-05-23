#!/usr/bin/env bash
# fetch-corpora.sh — shallow-clone the external Go repos listed in
# fixtures/repos.manifest into out/repos/<name>/, sha-pinned via the
# manifest's tag-or-sha column. Idempotent: skips repos already at the
# correct ref. (solov2-0k5h.2)
#
# Usage:
#   tools/loadtest/embed_models/scripts/fetch-corpora.sh
#
# Requires: git on PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MANIFEST="${ROOT}/fixtures/repos.manifest"
OUT="${ROOT}/out/repos"

mkdir -p "${OUT}"

if [[ ! -f "${MANIFEST}" ]]; then
    echo "fetch-corpora: missing manifest: ${MANIFEST}" >&2
    exit 1
fi

while IFS=$'\t' read -r name url ref; do
    # Skip blank lines and comments.
    [[ -z "${name}" || "${name}" =~ ^# ]] && continue

    dir="${OUT}/${name}"
    if [[ -d "${dir}/.git" ]]; then
        current="$(git -C "${dir}" describe --tags --always 2>/dev/null || echo "")"
        if [[ "${current}" == "${ref}" ]]; then
            echo "fetch-corpora: ${name} already at ${ref} — skip"
            continue
        fi
        echo "fetch-corpora: ${name} at ${current}, re-fetching ${ref}"
        rm -rf "${dir}"
    fi

    echo "fetch-corpora: clone ${url} @ ${ref} -> ${dir}"
    git clone --quiet --depth=1 --branch="${ref}" "${url}" "${dir}"
done < "${MANIFEST}"

echo "fetch-corpora: done. corpora at ${OUT}"
