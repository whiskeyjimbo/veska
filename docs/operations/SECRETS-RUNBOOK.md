---
title: "Secrets Runbook"
status: reference
last_reviewed: 2026-05-08
related: [SOLO-08, SOLO-13]
verified: true
verified_date: "2026-05-17"
---

# Secrets Runbook

When Veska's secret-scan fires, a credential has been written to
disk. Veska redacts the credential from its own mirror and
surfaces a finding. **Veska cannot un-publish a secret.** The
remaining steps are on you.

## What Veska does (automatically)

- **Redacts the credential** in `nodes` rows: `raw_content` is
  replaced with a placeholder; the `node_id` and `content_hash` are
  preserved so links don't break.
- **Evicts affected embeddings** from `node_embeddings` and
  `vec_nodes`; re-embeds with the redacted placeholder.
- **Emits a finding** with `source_layer='security'`,
  `severity='critical'`, and a stable `rule='secret_leak'` so it
  shows up in `eng_list_findings`.
- **Refuses to log the secret value** in `audit.jsonl` (the writer
  redacts known secret patterns at write time).

That is everything Veska can do alone. The credential is still
in your Git history, on every clone of the repo, and on every
mirror that has fetched from your remote.

## What you must do

### Step 1 — Rotate the credential. Now.

Rotation is the only step that genuinely reduces risk. Do this
first; everything else is hygiene.

- Revoke or rotate the leaked credential at the issuing system.
- Notify your security on-call per org policy.
- If the credential grants production access, treat as an incident.

### Step 2 — Rewrite Git history (optional, often not enough)

If the leaked credential is the kind you would re-use elsewhere
(a long-lived API key, a personal access token), rewrite history
to remove it. Veska cannot do this for you. Pick a tool:

```bash
# Option A — git-filter-repo (recommended)
pip install git-filter-repo
git clone --mirror git@github.com:org/repo.git repo-rewrite.git
cd repo-rewrite.git
echo '<the-secret>==><REDACTED>' > replacements.txt
git filter-repo --replace-text replacements.txt
git push --force-with-lease --all
git push --force-with-lease --tags
```

```bash
# Option B — BFG
java -jar bfg.jar --replace-text replacements.txt repo-rewrite.git
cd repo-rewrite.git
git reflog expire --expire=now --all && git gc --prune=now --aggressive
git push --force-with-lease --all
```

Coordination cost: every collaborator must re-clone or rebase.
Public forks are out of your control; if the repo is public, the
secret is permanent and Step 1 (rotation) is the only thing that
matters.

### Step 3 — Acknowledge in Veska

Findings are managed over MCP, not the CLI. Use the MCP tools:

- `eng_list_findings` (filter by `rule=secret_leak`) to locate the
  open finding.
- `eng_close_finding` with the `finding_id` and a reason such as
  "rotated, history rewritten".

`severity=critical` close requires `actor_kind=human`. The agent
cannot close a secret-leak finding for you.

## What `veska doctor` shows

```
$ veska doctor
...
warnings:
  - 1 open secret_leak finding (file: src/aws_client.go, age: 2h)
```

Open secret-leak findings older than 24h escalate the warning.
The intent is to prevent "I redacted the mirror and forgot to
rotate."

## Limits we are honest about

- **Public repos.** The secret is permanent. Rotate. Move on.
- **Forks and mirrors.** Out of scope for any local tool.
- **Binary blobs.** Veska does not parse binaries; mirror
  redaction applies only to indexed nodes.
- **CI logs and build artifacts.** Veska does not own these.
  Your CI retention policy does.
- **Off-disk leaks** (chat, ticket, PR comment). Veska cannot see
  these. Rotation is the answer.

The framing: the mirror redaction is hygiene. Rotation is the
risk reduction.
