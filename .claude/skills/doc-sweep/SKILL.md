---
description: "Iterate through every ready issue in a doc-verification epic, auditing and updating design docs, ADRs, subsystem docs, and feature docs. Each doc is checked for accuracy, completeness, DoD/acceptance criteria, and DESIGN.md alignment, then marked verified: true + verified_date. Surfaces nothing intermediate; only presents the final reviewed diff per issue."
arguments: [epic_id]
allowed-tools: [Bash, Read, Edit, Write, Agent]
---

# Doc Sweep Loop

Execute a disciplined verification loop over every ready issue in epic **$epic_id**.

## Pre-flight

1. `bd show $epic_id` - confirm it's an epic, get title.
2. `bd list --epic $epic_id --status=open --limit 500` - collect ready issues; cross-check with `bd blocked`.
3. Print `Epic $epic_id - N issues to process` + ordered ID list, then proceed silently.

---

## Per-Issue Loop

For each issue in dependency order (blockers first), run Steps 0–9. **Steps 1–9 are silent.** Step 0 is the only interactive step.

### Step 0 - Understand & Clarify

`bd show <issue_id>`. Read each doc file listed in the issue description. Apply the **blanket test**: would leaving every section as-is (no edits, no frontmatter change) pass the task description? If yes, the criteria are too thin.

**Surface a clarification block and pause** if ANY of:
- Can't determine whether a doc section is intentionally minimal or accidentally empty
- A `files:` entry doesn't exist on disk AND can't be confirmed as a valid planned path from DESIGN.md
- An architectural claim in a doc directly contradicts DESIGN.md with no ADR to explain the divergence
- Two+ reasonable interpretations of "complete" would lead to different doc content

```
┌─ Clarifying <issue_id>: <title> ──────────────────
│  My read: <1–2 sentences on what this doc covers and what's in question>
│
│  Before I audit I need answers to:
│    1. <specific question - intended scope, missing context, design intent>
│    2. <if applicable>
│
│  If my read is correct and the questions don't apply, say "proceed".
└───────────────────────────────────────────────────
```

Two sharp questions > five fuzzy ones. If unambiguous, skip block and note `(no clarification needed)` in the final report.

After the user responds, update description before claiming:
```bash
bd update <issue_id> --description="<original + clarifications>"
```

### Step 1 - Claim & Parse Checklist

```bash
bd update <issue_id> --claim && bd show <issue_id>
```

Extract every check from the issue's "Checks" or "What to check" section as a numbered checklist. Each check must describe an **observable doc property** (section present, link resolves, item checked) - not an editorial judgment.

### Step 2 - File Existence Audit

For every doc in scope, read its `files:` frontmatter list and verify each entry:

```bash
# For each path in files: list
ls <path> 2>&1
```

- **Exists**: mark pass.
- **Missing, but path is a plausible planned location** (matches DESIGN.md package structure): annotate the entry with `# planned` comment in frontmatter and mark pass.
- **Missing with no plausible basis**: flag as a finding - update the `files:` list to remove or correct it, and note the correction in the issue report.

Skip this step for tasks that cover no files: frontmatter (DESIGN.md, ADRs, top-level docs).

### Step 3 - Section Audit & Update

For each doc file, run the full checklist from the issue description. For each failing check:

1. If the section is **absent**: add it using the relevant template from `docs/templates/`.
2. If the section is **present but empty or too thin**: fill it in from the codebase, DESIGN.md, and ADRs. Do not invent design intent - derive only from observable code and existing docs.
3. If the section **contradicts DESIGN.md**: correct the doc to match, unless there is an ADR explaining the divergence. If there is an ADR, add a cross-reference.

**Archived feature rules:**
- All DoD items must be `[x]` checked. If an item is genuinely incomplete in the shipped feature, mark it `[ ] - accepted-incomplete: <reason>` rather than silently leaving it unchecked.
- Empty `## Capability` sections are acceptable only if `status: shipped` in frontmatter. Do not fill them in if the shipped behavior is clear from surrounding sections.

**Scope creep rule:** If a doc reveals a design gap (something that should exist but doesn't, in code or docs), file a new bead rather than expanding the current issue:
```bash
bd create --type=task --title="<gap description>" --description="<what was found and where>"
```

### Step 4 - Cross-Reference Check

For each updated doc, verify that all internal cross-references resolve:

```bash
# Check all markdown links to other docs
grep -oP '\[.*?\]\(\K[^)]+' <file> | grep -v '^http' | while read ref; do
  ls "$(dirname <file>)/$ref" 2>/dev/null || echo "BROKEN: $ref"
done
```

Also verify:
- `related_adrs:` frontmatter entries match actual ADR filenames in `docs/decisions/`.
- `depends_on:` IDs use the canonical format from the `id:` field of the referenced feature doc.
- DESIGN.md section references (`§N`) exist in the current DESIGN.md.

Fix every broken reference inline.

### Step 5 - DoD Checkbox Audit (Archived Only)

For archived feature docs: confirm every `- [ ]` DoD item has been addressed. Accept three states only:
- `- [x] item` - completed
- `- [ ] item - accepted-incomplete: <reason>` - knowingly skipped with explanation
- `- [ ] item` with no annotation - **not acceptable**; must be resolved to one of the above

For active feature docs: DoD items may remain unchecked (they represent future work). Verify only that the items are specific and measurable - not structural descriptions like "add a function for X".

### Step 6 - Quality Review (Sub-agent)

Produce a diff of all changes made in Steps 2–5 (`git diff -- <changed files>`), then spawn an Agent with this exact prompt (fill in the diff and doc type):

> Review the following doc diff for a <doc type: feature doc / ADR / subsystem design doc / top-level doc>. Check: (a) **accuracy** - does the Capability section describe observable feature behavior, not implementation details? (b) **DoD quality** - are items specific, measurable, and describe outcomes a test could verify, or are they vague/structural? (c) **broken references** - do all file paths, ADR references, and DESIGN.md section citations in the updated content resolve? (d) **gaps** - is there any required section still missing or thin enough that an implementer would have to guess? Return a numbered list of findings. If nothing found, return "LGTM".

Apply every finding. If a finding requires a judgment call about design intent, file a bead and note it as scope creep.

### Step 7 - Add Verified Frontmatter

Once all checks pass, add to each doc's YAML frontmatter:

```yaml
verified: true
verified_date: "<today's date YYYY-MM-DD>"
```

If no frontmatter block exists, add one at the top of the file. If one exists, append the two fields.

### Step 8 - Commit

```bash
git add <changed files - explicit, no git add -A>
git commit -m "docs(<area>): verify and update <short description>"
```

One-line conventional-commit. `<area>` is the subsystem name (e.g., `ingest`, `mcp`, `vuln`) or `design`/`adr` for foundation docs. No trailers.

### Step 9 - Close

```bash
bd close <issue_id> --reason="verified: <one line summary of what changed>"
```

---

## Final Report (per issue)

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Issue:    <id> - <title>
Status:   ✓ CLOSED  |  ✗ BLOCKED (reason)
Clarity:  (no clarification needed) | Asked N questions - answers updated description
Docs:     N files updated  |  N files verified as-is
Fixes:    <summary: broken refs fixed, sections added, DoD items resolved, etc.>
Commit:   <short sha> <message>
Checks:
  [x] check 1
  [x] check 2
  [ ] check 3 - SKIPPED (reason, new bead: <id>)
Review:   <"LGTM" or numbered findings addressed>
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

## Blocked Format

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Issue:    <id> - <title>
Status:   ✗ BLOCKED - <reason>
Detail:
  <what was found that requires human judgment, max 10 lines>
New beads: <ids filed, or none>
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

Release claim: `bd update <id> --status=open`, continue.

## Post-Epic Summary

```
═══════════════════════════════════════════════
Epic $epic_id complete
Closed:   N / M issues
Blocked:  K issues (listed above)
New beads filed for scope creep: <ids or none>
═══════════════════════════════════════════════
```

```bash
bd dolt push && git pull --rebase && git push
```
