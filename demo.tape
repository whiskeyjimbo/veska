# Veska README demo — record with VHS:  https://github.com/charmbracelet/vhs
#
#   go install github.com/charmbracelet/vhs@latest   # or: brew install vhs
#   vhs demo.tape                                     # writes docs/manual/assets/demo.gif
#
# Preconditions (so the queries return instantly on screen):
#   - `veska` is on PATH (make install)
#   - the daemon is running and THIS repo is registered + fully indexed
#     (veska repo add . ; veska service start ; wait for embeds to drain)
#
# Keep the recording to a handful of beats — the story is "natural-language
# question in, exact file:line answer out, no grep."

Output docs/manual/assets/demo.gif

Set Shell bash
Set FontSize 18
Set Width 1200
Set Height 720
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms

Hide
Type "clear" Enter
Show

# Beat 1 — semantic search returns grounded spans, ranked by tier.
Type "# Ask in plain English. Get exact file:line spans, not grep guesses." Sleep 800ms Enter
Sleep 600ms
Type 'veska search "parse config"' Sleep 400ms Enter
Sleep 3500ms

# Beat 2 — jump from a result to the exact symbol.
Type "# Know the name? Exact, deterministic lookup." Sleep 800ms Enter
Sleep 400ms
Type "veska symbol Load" Sleep 400ms Enter
Sleep 3000ms

# Beat 3 — trace the call graph without opening a file.
Type "# Trace what it reaches — straight from the parsed graph." Sleep 800ms Enter
Sleep 400ms
Type "veska calls Load" Sleep 400ms Enter
Sleep 3500ms

# Closing beat — the whole thing is local + zero-setup.
Type "# All local. No cloud, no API key. Served to your AI agent over MCP." Sleep 1200ms Enter
Sleep 2500ms
