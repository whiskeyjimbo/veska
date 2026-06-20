#!/usr/bin/env bash
# Records the README demo. Browser-free path (works where VHS's headless Chrome
# can't launch, e.g. Ubuntu 24.04 unprivileged-userns restriction):
#
#   asciinema rec --overwrite --command "bash demo.sh" /tmp/demo.cast
#   # widen the recorded terminal, then render to GIF:
#   python3 -c "import json,sys; l=open('/tmp/demo.cast').read().split(chr(10)); \
#     h=json.loads(l[0]); h['width']=130; h['height']=30; l[0]=json.dumps(h); \
#     open('/tmp/demo.cast','w').write(chr(10).join(l))"
#   agg --font-size 18 /tmp/demo.cast docs/manual/assets/demo.gif
#
# A VHS tape (demo.tape) renders the same beats where Chrome is available.
# Run `bash demo.sh` directly to preview the beats live.
set -u
cd "$(dirname "$0")"
export PATH="$PWD/bin:$PATH"

say()  { printf '\033[2;37m%s\033[0m\n' "$1"; sleep 1.0; }
demo() { printf '\033[1;32m$\033[0m \033[1m%s\033[0m\n' "$1"; sleep 0.5; eval "$1"; echo; sleep 2.2; }

sleep 0.5
say "# Ask in plain English -> exact file:line spans, not grep guesses."
demo 'veska search "promote staged changes atomically"'

say "# Found it by behavior. Now pin the exact symbol."
demo 'veska symbol Promoter.Promote'

say "# Trace what it reaches -> straight from the parsed graph, no file opened."
demo 'veska calls Promoter.Promote'

say "# All local. No cloud, no API key. Served to your AI agent over MCP."
sleep 1.5
