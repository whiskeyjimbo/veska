# Backup & restore

Everything Veska knows lives under `~/.veska/`, so backup is a single command
and restore is its inverse. Backups are an online SQLite snapshot plus a tarball
of supporting files.

## Create a backup

```sh
veska backup create
```

Writes a backup tarball to `$VESKA_HOME/backups` (override with `--output-dir`).
Safe to run while the daemon is up — it uses an online SQLite snapshot.

## List & prune

```sh
veska backup list        # newest first (--json for machine output)
veska backup verify      # check a backup tarball's integrity
veska backup prune       # apply the retention policy, delete old backups
```

`backup list` reads `$VESKA_HOME/backups`, falling back to `~/.veska-backups`.

## Restore

!!! warning "Stop the daemon first"
    Restore overwrites the live database. The daemon **must be stopped** before
    restoring:

    ```sh
    veska service stop
    ```

```sh
veska restore /path/to/backup.tar.gz   # explicit tarball
veska restore --latest                 # newest backup in $VESKA_HOME/backups
veska restore --pre-migration          # newest auto-pre-migration snapshot
```

Veska automatically takes a **pre-migration** snapshot before schema migrations,
so `--pre-migration` is your escape hatch if an upgrade goes wrong.

After restoring, start the daemon again:

```sh
veska service start
veska doctor status
```

## Health

```sh
veska doctor backup     # backup subsystem status
```
