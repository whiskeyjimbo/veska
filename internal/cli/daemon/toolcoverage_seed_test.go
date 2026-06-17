// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package daemon

// Operational seed-state inserts for the tool-coverage harness.
// These rows are the literal starting state the finding / suppression / task /
// alias tools assert against - NOT parse output. The coverage package's seed
// facts carry only the test-meaningful columns; this file fills the remaining
// NOT NULL / CHECK columns (finding_id, branch, source_layer, created_at,
// actor_id, actor_kind∈{human,agent,system}, suppression scope/target, task
// title) so the inserts satisfy the schema. NodeKey anchors are resolved to
// node IDs via the harness root so no raw sha256 is ever written here.
// The fixture repos themselves are already inserted (with their real root +
// module path) by indexRepo; seedRepos in the coverage facts is therefore not
// re-inserted as repos rows - the aliases below reference the already-present
// fixture repo IDs.

import (
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp/coverage"
)

// seedOperationalState inserts the coverage manifest's seed-state facts:
// aliases, tasks, findings, and suppressions. Repos are already present.
func (h *toolHarness) seedOperationalState() {
	h.t.Helper()
	facts := coverage.Manifest()
	h.seedAliases(facts.Aliases)
	h.seedTasks(facts.Tasks)
	h.seedFindings(facts.Findings)
	h.seedSuppressions(facts.Suppressions)
}

func (h *toolHarness) seedAliases(aliases []coverage.AliasFact) {
	for _, a := range aliases {
		h.execSeed(
			`INSERT INTO repo_aliases (name, repo_id) VALUES (?, ?)`,
			a.Alias, a.RepoID,
		)
	}
}

func (h *toolHarness) seedTasks(tasks []coverage.TaskFact) {
	now := time.Now().UnixMilli()
	for _, tk := range tasks {
		active := 0
		if tk.Active {
			active = 1
		}
		h.execSeed(
			`INSERT INTO tasks (task_id, repo_id, title, active, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			tk.TaskID, tk.RepoID, tk.Description, active, now,
		)
	}
}

func (h *toolHarness) seedFindings(findings []coverage.FindingFact) {
	now := time.Now().UnixMilli()
	for i, f := range findings {
		nodeID := string(h.ResolveID(f.RepoID, f.Anchor))
		var closedAt any
		if f.State == "closed" {
			closedAt = now
		}
		h.execSeed(
			`INSERT INTO findings
			 (finding_id, branch, repo_id, node_id, file_path, severity,
			  source_layer, rule, message, state, closed_at, created_at,
			  actor_id, actor_kind)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			seedID("finding", i), coverage.FixtureBranch, f.RepoID, nodeID, f.Anchor.Path,
			f.Severity, "harness", f.Rule, f.Message, f.State, closedAt, now,
			"tool-coverage-harness", "agent",
		)
	}
}

func (h *toolHarness) seedSuppressions(supps []coverage.SuppressionFact) {
	now := time.Now().UnixMilli()
	for i, s := range supps {
		// scope=node, target=the anchored node ID - the suppression tools key
		// on (target, branch); see idx_suppressions_target.
		target := string(h.ResolveID(s.RepoID, s.Anchor))
		h.execSeed(
			`INSERT INTO suppressions
			 (suppression_id, scope, target, branch, rule, reason, created_at,
			  actor_id, actor_kind)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			seedID("suppression", i), "node", target, coverage.FixtureBranch,
			s.Rule, s.Reason, now, "tool-coverage-harness", "agent",
		)
	}
}

// seedID builds a deterministic primary key for a seeded row, e.g.
// "seed-finding-0". Determinism keeps the coverage harness able to assert on the ID.
func seedID(kind string, i int) string {
	return "seed-" + kind + "-" + itoa(i)
}

// itoa renders a small non-negative int without pulling strconv into the seed
// file's import set for a single call site.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
