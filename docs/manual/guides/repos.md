# Managing repositories

Veska tracks a set of registered git repositories. Registering a repo installs
its post-commit hook and kicks off the first (cold) scan; from then on, commits
drive promotion automatically.

## Add a repo

```sh
veska repo add /path/to/your/repo --wait
```

`--wait` blocks until the cold scan completes and prints live progress, so your
first search is hot. Without it the scan runs in the background - tail
`~/.veska/logs/daemon.log` for `cold scan: complete`.

`repo add` also accepts a **git URL**, which Veska clones into its cache tier and
marks ephemeral. Registering a local repo installs the git post-commit hook with
an absolute path to the `veska` binary.

## List & inspect

```sh
veska repo list                 # registered repos
veska repo current              # the repo the current directory belongs to
veska repo show <id-or-short>   # one repo's details
```

Add `--json` to `current` / `show` for machine-readable output.

## Aliases

Bind a human-friendly name to a repo. The **new name comes first**, the existing
repo second - same order as `git remote add <name> <url>`:

```sh
veska repo alias lib a1b2          # alias repo a1b2… to "lib"
veska repo alias lib c3d4 --force  # repoint an existing alias
veska repo unalias lib             # remove an alias
```

## Remove a repo

```sh
veska repo remove /path/to/repo     # deregister one repo, remove its hooks
veska repo remove --missing         # remove repos whose root no longer exists
veska repo remove --all --yes       # remove everything (scripts need --yes)
veska repo remove --dry-run …       # preview without changing anything
```

## Re-scan

To force a fresh scan of an already-registered repo (e.g. after a model swap):

```sh
veska reindex /path/to/your/repo
```

Safe while the daemon is up - it dispatches through the daemon's
`eng_reindex_repo` tool, so editor MCP connections aren't interrupted. With the
daemon stopped, it falls back to a direct in-process reparse.
