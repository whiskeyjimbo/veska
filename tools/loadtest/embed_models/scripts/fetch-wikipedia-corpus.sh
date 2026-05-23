#!/usr/bin/env bash
# fetch-wikipedia-corpus.sh — pulls plain-text extracts of the Wikipedia
# articles listed in fixtures/wikipedia_titles.txt and writes one
# markdown file per article into out/wikipedia-tech/. Used by the
# embed-models bench to evaluate models on GENUINE natural prose, since
# our other prose corpora (veska-docs, cobra-docs) are really dense
# technical documentation full of library identifiers (solov2-0k5h.7).
#
# Idempotent: skips files already on disk. Content is CC-BY-SA 3.0 (the
# Wikipedia license); out/ is gitignored to avoid redistribution issues.
#
# Usage:
#   tools/loadtest/embed_models/scripts/fetch-wikipedia-corpus.sh
#
# Requires: curl, python3 on PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TITLES="${ROOT}/fixtures/wikipedia_titles.txt"
OUT="${ROOT}/out/wikipedia-tech"

mkdir -p "${OUT}"

if [[ ! -f "${TITLES}" ]]; then
    echo "fetch-wikipedia: missing ${TITLES}" >&2
    exit 1
fi

while IFS= read -r title; do
    # skip blanks + comments
    [[ -z "${title// }" || "${title}" =~ ^# ]] && continue

    # slug for filename: lowercase a-z0-9, runs of others -> '-', trim edges.
    slug=$(python3 -c "
import re, sys
s = sys.argv[1].lower()
s = re.sub(r'[^a-z0-9]+', '-', s).strip('-')
print(s)
" "${title}")
    out_file="${OUT}/${slug}.md"

    if [[ -s "${out_file}" ]]; then
        echo "fetch-wikipedia: ${title} cached — skip"
        continue
    fi

    echo "fetch-wikipedia: ${title}"

    # Hit the MediaWiki API for a plain-text extract with section markers
    # preserved (== Heading == form). Python then converts those to ATX
    # markdown headings so the prose corpus loader can split sections.
    encoded=$(python3 -c "
import urllib.parse, sys
print(urllib.parse.quote(sys.argv[1].replace(' ', '_')))
" "${title}")
    url="https://en.wikipedia.org/w/api.php?action=query&prop=extracts&exsectionformat=wiki&explaintext=1&titles=${encoded}&format=json&redirects=1"

    # Wikipedia requires a contact-info User-Agent and rate-limits aggressively
    # without one. A 0.5s sleep between requests stays under the
    # one-per-second courtesy guideline.
    body=$(curl -fsSL --max-time 20 \
        -H 'User-Agent: veska-embed-models-bench/1.0 (solov2-0k5h.7; https://github.com/whiskeyjimbo/veska)' \
        -H 'Accept: application/json' \
        "${url}" || true)
    sleep 0.5
    if [[ -z "${body}" ]]; then
        echo "fetch-wikipedia: empty response for ${title} — skip"
        continue
    fi

    extracted=$(printf '%s' "${body}" | python3 -c "
import json, re, sys
try:
    data = json.load(sys.stdin)
except Exception as e:
    sys.exit(0)
pages = data.get('query', {}).get('pages', {})
for _, p in pages.items():
    text = p.get('extract', '')
    if not text:
        continue
    out = []
    for line in text.split('\n'):
        m = re.match(r'^(=+)\s*(.+?)\s*\1\s*$', line)
        if m:
            level = len(m.group(1))
            # Cap depth at H6 so we stay valid markdown; map == -> ##.
            level = min(level, 6)
            out.append('#' * level + ' ' + m.group(2))
        else:
            out.append(line)
    print('\n'.join(out))
")
    if [[ -z "${extracted// }" ]]; then
        echo "fetch-wikipedia: empty extract for ${title} — skip"
        continue
    fi

    {
        echo "# ${title}"
        echo
        echo "${extracted}"
    } > "${out_file}"
done < "${TITLES}"

echo "fetch-wikipedia: done. ${OUT}"
