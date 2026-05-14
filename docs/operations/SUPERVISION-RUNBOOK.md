---
id: SOLO-OPS-SUPERVISION
title: "Supervision Runbook — Install, Upgrade, Crash-loop"
status: draft
last_reviewed: 2026-05-08
related: [SOLO-03, SOLO-13, CONFIG-SURFACE]
---

# Supervision Runbook

How `engram-daemon` is supervised on a developer machine. The
canonical design is SOLO-03 §5; this document is operator-facing
detail — the unit-file shape, the `engram service` subcommands,
the recovery steps when something goes wrong.

## 1. Install

`engram service install` writes the unit file for the host
platform and registers it. The command is idempotent: running it
twice does not break anything.

### macOS — launchd

Writes `~/Library/LaunchAgents/com.engram.daemon.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>          <string>com.engram.daemon</string>
    <key>ProgramArguments</key>
    <array>
      <string>/Users/USER/.engram/bin/engram-daemon</string>
    </array>
    <key>RunAtLoad</key>      <true/>
    <key>KeepAlive</key>
    <dict>
      <key>SuccessfulExit</key> <false/>
    </dict>
    <key>StandardOutPath</key> <string>/Users/USER/.engram/logs/daemon.log</string>
    <key>StandardErrorPath</key><string>/Users/USER/.engram/logs/daemon.log</string>
    <key>ExitTimeOut</key>     <integer>30</integer>
    <key>EnvironmentVariables</key>
    <dict>
      <key>ENGRAM_HOME</key>   <string>/Users/USER/.engram</string>
    </dict>
  </dict>
</plist>
```

Loaded with `launchctl bootstrap gui/$(id -u)
~/Library/LaunchAgents/com.engram.daemon.plist`.

`KeepAlive.SuccessfulExit = false` means launchd restarts on
non-zero exit but **not** on a clean exit (code 0). Exit code 78
(crash-loop breaker; SOLO-03 §5.6) is treated as failure by
default — the breaker's marker file is what stops the loop, not
the exit code.

### Linux — systemd --user

Writes `~/.config/systemd/user/engram-daemon.service`:

```ini
[Unit]
Description=Engram daemon (solo)
After=default.target

[Service]
Type=simple
ExecStart=%h/.engram/bin/engram-daemon
Restart=on-failure
RestartSec=2
StartLimitIntervalSec=600
StartLimitBurst=5
Environment=ENGRAM_HOME=%h/.engram
StandardOutput=append:%h/.engram/logs/daemon.log
StandardError=append:%h/.engram/logs/daemon.log
TimeoutStopSec=30
SuccessExitStatus=0

[Install]
WantedBy=default.target
```

Enabled with `systemctl --user enable --now engram-daemon`.
`StartLimitIntervalSec` / `StartLimitBurst` mirror the daemon's
own breaker (5 restarts / 10 min) for defense-in-depth.

### Linux without `systemd --user` (Alpine, NixOS w/o systemd-user, devcontainers, default WSL2)

Many real Linux installs do not have `systemd --user` enabled.
The supervisor for these is the **built-in `engram supervise`
subcommand** — a Go-side restart loop in the same binary,
sharing the crash-loop breaker (§4) with the launchd / systemd
paths. There is no shipped shell script; the prior 18-line
`engram-supervise.sh` is retired.

```
engram supervise [--pidfile=$ENGRAM_HOME/state/supervise.pid]
```

Properties:

- Exits 0 on a clean child exit (`engram-daemon` returned 0).
- Exits 78 on a child exit-78 (terminal — schema mismatch,
  sqlite-vec missing, etc.) without restarting. The
  supervisor's parent (the user's autostart hook) sees the 78
  and stops trying.
- On any other non-zero child exit, restarts the child after
  2s up to the breaker's window (5 in 10 min by default;
  CONFIG-SURFACE `[supervisor]`).
- Maintains a PID file the MCP shim reads at startup to detect
  whether a supervisor is registered (SOLO-03 §3.1).
- Forwards SIGTERM to the child for clean stop.

`engram service install` on a no-systemd-user host writes a
shell-rc snippet that invokes `engram supervise` and prints the
exact line for the user to add to their autostart mechanism
(.bashrc/.zshrc, tmux startup, an init.d entry, a desktop-
environment autostart entry). The installer does not edit shell
files itself; it prints what to add.

### Other platforms

- **Windows.** Not supported. WSL2 falls under the Linux
  paths above (typically the no-systemd-user fallback).
- **Other unices.** `engram service install` falls through to the
  no-systemd-user helper above; user wires it into whatever
  supervisor they have.

## 2. Verify

```
engram service status      # supervisor's view (registered? running?)
engram doctor service      # daemon's view (PID, recent restarts, broken marker?)
```

Both should agree. Disagreement means the user manually started
an `engram-daemon` outside the supervisor — see §5.

## 3. Upgrade

The standard flow:

```
engram upgrade <new-binary-path>     # stages binaries, atomic mv into place
engram service restart               # asks supervisor to stop+start the daemon
```

Or in one command: `engram upgrade --restart`.

If the new daemon's required schema is newer than what's on
disk, the migration runner (SOLO-08 §10) takes its own
pre-migration snapshot and applies the pending migrations in
order. The user does not need to run `engram backup create`
manually before an upgrade.

If a migration fails, the daemon refuses to start with exit 78
and a pointer to the verified pre-migration snapshot. Recovery:

```
# 1. Read the log to identify the failure
tail -200 ~/.engram/logs/daemon.log | grep migration

# 2a. If you can fix the migration: install the patched binary, then
engram service restart

# 2b. If you must roll back the upgrade: install the prior binary
#     (whose max_schema covers the on-disk version), then
engram service restart

# 2c. If 2a and 2b are both blocked: restore from the pre-migration
#     snapshot the runner took before the failing migration
engram daemon stop
engram restore --pre-migration   # one command — auto-selects the most recent
                                  # pre-migration snapshot, verifies it,
                                  # renames the live DB to .replaced-<ts>/,
                                  # extracts the snapshot, prints the
                                  # binary version that pairs with that schema
engram service restart
```

A schema-mismatch refusal (binary too new or too old for the
on-disk schema) follows the same pattern: install a binary whose
`min_schema..max_schema` range covers the on-disk version.

## 4. Crash-loop recovery

Symptom: a desktop notification ("Engram daemon stopped
(crash-loop). Run: engram doctor reset-crash-loop") on macOS or
Linux with `notify-send`; on platforms without either, the next
`engram` invocation prints a one-line banner pointing at
`~/.engram/state/CRASH-LOOP-TRIPPED.txt`. The editor sees the
MCP socket close with `ErrDaemonNotRunning`; `engram service
status` shows the supervisor gave up; `engram doctor service`
shows `~/.engram/state/broken` exists. The notification path is
defined in SOLO-03 §5.6.

The breaker tripped because the daemon restarted ≥ 5 times in
the last 10 minutes. Common causes:

| Cause | Where to look |
|---|---|
| RSS > 4 GiB hard cap | `~/.engram/logs/daemon.log` for repeated `engram_code: "ErrMemoryHardCap"` lines. Likely a refactor storm or a massive cold-scan. |
| Migration failure | Same log, `engram_code: "ErrMigrationFailed"`. Inspect schema drift. |
| sqlite-vec extension missing | `engram_code: "ErrVecExtensionMissing"`. Reinstall the extension. |
| Disk full | `engram doctor storage` exit 2. Free space. |
| `~/.engram/` on NFS or other unsupported filesystem | `engram doctor fs` exit 2 with `ErrUnsupportedFS`. SQLite + WAL has known correctness issues on NFS. Move `~/.engram/` (`ENGRAM_HOME`) to a local filesystem. |
| `daemon_state.restart_count` row missing or invalid | `engram doctor` reports `ErrCounterInvalid`. The daemon treats a missing/invalid row as zero on next start (re-creates the row in its initial-boot transaction), logs a warning, and continues. SQLite handles the atomicity; corruption of this row alone does not require manual file editing. |

Recovery:

```
# 1. Read the recent log to identify the cause
tail -100 ~/.engram/logs/daemon.log

# 2. Address the cause (free disk, reinstall sqlite-vec, etc.)

# 3. Clear the breaker
engram doctor fix       # offers to clear the marker after summarising the cause

# 4. Ask the supervisor to start
engram service restart
```

If the cause is a legitimate memory ceiling on a huge repo, raise
`memory.hard_cap_gib` in `~/.engram/config.toml` before clearing
the breaker. (Caveat: the 4 GiB cap is a soft signal that
something is wrong, not a hard physical limit. Raising it
indefinitely masks bugs.)

## 5. Uninstall and reset

```
engram service uninstall   # remove unit file; supervisor forgets the daemon
engram daemon stop         # if still running outside supervisor
rm -rf ~/.engram           # full reset; loses all promoted state
```

`engram service uninstall` does not delete data or logs. Pair it
with the `rm -rf` only when you actually want a fresh start.

## 6. What this runbook does not cover

- The daemon's internal failure modes during normal operation —
  see SOLO-13 §4.
- The audit log — see SOLO-08 §3.5.
- Ollama / embedder issues — see `engram doctor embedder` and
  SOLO-13.
- Backup creation and verification — see `engram backup` and
  `engram doctor backup`.
