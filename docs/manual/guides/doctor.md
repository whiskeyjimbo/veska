# Diagnostics with doctor

`veska doctor` is the operator's window into the daemon. It's the one surface you
reach for when something looks wrong - or to confirm things are healthy.

## Start here

```sh
veska doctor status     # overall health rollup across all subsystems
```

Add `--json` for machine-readable output, or `-v` / `--verbose` to include
failed queue rows inline.

## Subsystem checks

Each subsystem has its own focused check:

| Command | What it reports |
|---|---|
| `veska doctor status` | Overall health rollup across all subsystems |
| `veska doctor service` | OS-service health (installed? running?) |
| `veska doctor storage` | Filesystem storage metrics for `~/.veska/` |
| `veska doctor embedder` | Which embedder is elected, and its health |
| `veska doctor post_promotion_queue` | Backlog of post-commit embed/check work |
| `veska doctor pipelines` | Promotion pipeline health |
| `veska doctor config` | Effective configuration |
| `veska doctor identity` | Actor-attribution / identity tier |
| `veska doctor egress` | Outbound-connection posture |
| `veska doctor backup` | Backup status |
| `veska doctor wiki_render` | Wiki render health |
| `veska doctor savings` | Inline-snippet token savings (also `veska savings`) |
| `veska doctor bundle` | Bundle the above into one diagnostic report |

## When things look stuck

- **Search returns `[]` or `degraded_reasons`** - embeddings are still catching
  up. Check `veska doctor post_promotion_queue` and `eng_get_status`'s
  `pending_embeds`. See **[Semantic search & embeddings](../concepts/embeddings.md)**.
- **Daemon won't stay up** - check `veska doctor service`, then the logs at
  `~/.veska/logs/daemon.log`. If a crash-loop guard tripped:

    ```sh
    veska doctor reset-crash-loop
    ```

- **Collecting a report for a bug** - `veska doctor bundle` gathers the full
  diagnostic set in one shot.

!!! tip "doctor is the operator surface"
    By design, `veska doctor` is the thing an operator actually sees. If a
    subsystem can fail, it has a doctor check - start there before reading logs.
