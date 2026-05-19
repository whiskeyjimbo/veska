---
id: SOLO-MILESTONES
title: "Milestones — Index"
status: draft
last_reviewed: 2026-05-18
verified: true
verified_date: "2026-05-18"
related: [SOLO-14]
---

# Milestones

Each milestone has a goal, exit gates (measured, not aspirational),
ordered epics with DoD, and TDD-sized sub-issues. Beads IDs follow
`veska:m<N>.<NN>-<slug>`.

| Milestone | Theme | Doc |
|---|---|---|
| M0 | Substrate spike (entry gate to M1) | [`closed/M0.md`](closed/M0.md) ✓ closed |
| M1 | Substrate foundation | [`closed/M1.md`](closed/M1.md) ✓ closed |
| M2 | Identity, observability, plugin scaffolding | [`closed/M2.md`](closed/M2.md) ✓ closed |
| M3 | Pipelines and embedder | [`closed/M3.md`](closed/M3.md) ✓ closed |
| M4 | Wiki mechanical kinds | [`closed/M4.md`](closed/M4.md) ✓ closed |
| M5 | Optional review pipeline | [`closed/M5.md`](closed/M5.md) ✓ closed |
| M6 | Cutover | [`closed/M6.md`](closed/M6.md) ✓ closed |
| M7 | Vuln + secrets scanning | [`closed/M7.md`](closed/M7.md) ✓ closed |

The roadmap-level summary is [SOLO-14](../design/14-roadmap/README.md).
That doc owns the goal-and-exit-gate framing; these per-milestone
docs own the epic decomposition and the work breakdown.

## Working a milestone

1. Open the milestone doc.
2. Pick an unclaimed epic. Read its DoD and acceptance criteria.
3. Each sub-issue is sized for one TDD iteration: tests-first →
   implement → race-clean → vet/lint → fmt → commit.
4. When all sub-issues are closed and the DoD ticks, close the epic.
5. When all epics close and the exit gates are measured, close the
   milestone.

Exit gates are measurements. A milestone closes when the numbers
exist, not when the code looks done.
