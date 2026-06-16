#!/usr/bin/env bash
# install.sh - copy the veska binaries into a user-writable bin dir.
#
# Two modes:
#   1. Inside a release tarball, where ./bin/{veska,veska-daemon,veska-mcp}
#      sit next to this script - copies those binaries.
#   2. Run from a clone (`scripts/install.sh` after `make build`) - same
#      bin/ layout, same copy. The Makefile's `make install` target is the
#      preferred entry point for this case.
#
# Destination resolution, in order:
#   - $VESKA_INSTALL_DIR (if set; created if missing).
#   - $XDG_BIN_HOME (if set).
#   - $HOME/.local/bin (default; mkdir -p when absent).
#
# /usr/local/bin is NOT used as a default: per-user installs avoid sudo,
# match the GitHub-releases / homebrew-cellar conventions, and stay out
# of /usr/local's path until the user opts in.
#
# Exits non-zero with a stderr message on any copy failure. PATH advice
# is printed when the destination isn't on PATH (the binary works, but
# the user has to type the full path until they fix this).

set -euo pipefail

err() { printf 'install.sh: %s\n' "$*" >&2; }

# Locate the bin/ dir relative to this script. Works whether the script
# is invoked as `scripts/install.sh` (clone) or `./install.sh` (extracted
# tarball, where install.sh sits next to bin/).
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
src_bin=""
for candidate in "$script_dir/bin" "$script_dir/../bin"; do
    if [[ -x "$candidate/veska" && -x "$candidate/veska-daemon" && -x "$candidate/veska-mcp" ]]; then
        src_bin="$(cd "$candidate" && pwd)"
        break
    fi
done
if [[ -z "$src_bin" ]]; then
    err "could not find veska binaries (looked in $script_dir/bin, $script_dir/../bin)"
    err "run \`make build\` first, or extract the release tarball into its own directory and run ./install.sh from there"
    exit 1
fi

# Pick destination directory.
if [[ -n "${VESKA_INSTALL_DIR:-}" ]]; then
    dest="$VESKA_INSTALL_DIR"
elif [[ -n "${XDG_BIN_HOME:-}" ]]; then
    dest="$XDG_BIN_HOME"
else
    dest="${HOME}/.local/bin"
fi

mkdir -p "$dest"

# Install the one veska binary; veska-daemon and veska-mcp are symlinks to
# it (solov2-brw6 consolidated the three argv[0] personas into one binary).
cp -f "$src_bin/veska" "$dest/veska"
chmod 0755 "$dest/veska"
ln -sf veska "$dest/veska-daemon"
ln -sf veska "$dest/veska-mcp"

printf 'installed veska to %s\n' "$dest"

# PATH sanity check: only warn - the user can still invoke "$dest/veska"
# directly, and editing shell config without asking is overreach.
case ":${PATH:-}:" in
    *":$dest:"*) ;;
    *)
        printf '\n'
        printf 'NOTE: %s is not in PATH. Add it (e.g. in ~/.bashrc / ~/.zshrc):\n' "$dest"
        printf '  export PATH="%s:$PATH"\n' "$dest"
        ;;
esac

printf '\nnext: veska init -y && veska service install && veska service start\n'
