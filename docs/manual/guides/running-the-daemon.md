# Running the daemon

The daemon (`veska-daemon`) is the long-running process that owns everything
under `~/.veska/`. You can background it for a quick trial, but for real use run
it as an OS service so it survives reboots and crashes.

## Quick trial: background it

```sh
./bin/veska-daemon &
```

Fine for kicking the tyres. No auto-restart, no managed logs - when your shell
exits, so does the daemon.

## Real use: run it as a service

`veska service` manages the daemon as an OS service - **systemd `--user`** on
Linux, **launchd** on macOS:

```sh
veska service install     # register the service (one time)
veska service start       # start it now (and on boot)
veska service status      # is it running?
veska service restart     # restart (e.g. after editing config.toml)
veska service stop        # stop it
veska service uninstall   # remove the service definition
```

As a service the daemon **starts on boot, auto-restarts on crash, and logs under
`~/.veska/logs/`**.

!!! tip "Config changes need a restart"
    The daemon reads `~/.veska/config.toml` at start. After editing it (for
    example the `[vuln_source]` block), run `veska service restart` for the
    change to take effect.

## Health & logs

```sh
veska doctor status              # overall health rollup
veska doctor service             # service-specific health
tail -f ~/.veska/logs/daemon.log # live logs
```

Look for `cold scan: complete` in the log after registering a repo - that's when
the first index is hot. See **[Diagnostics with doctor](doctor.md)** for reading
the health output, and **[Connecting your editor](editor-setup.md)** to wire an
MCP client to the running daemon.

## Crash-loop recovery

If the supervisor trips a crash-loop guard, clear it with:

```sh
veska doctor reset-crash-loop
```
