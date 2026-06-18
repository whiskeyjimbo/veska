// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package review

import (
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// maxReviewAttempts mirrors the queue.Poller retry limit (: 3
// attempts then state='failed'). The poller increments row.Attempts before
// each Handle call, so the FINAL failing attempt is the one observing
// row.Attempts >= maxReviewAttempts - that is when the Handler emits the
// review-pipeline-failure Finding.
const maxReviewAttempts = 3

// FailureRule is the rule string carried by every review-pipeline-failure
// Finding. It is shared by the Handler (emit), the eng_close_finding handler
// (close-flips-row), and the doctor post-promotion-queue probe (invariant
// check) so all three agree on the same finding identity.
const FailureRule = "review-pipeline-failure"

// FailureFindingID derives the branch-stable finding_id for a
// review-pipeline-failure Finding anchored on a promotion commit.
// The Finding anchors its node_id on gitSHA and sets no WithFindingKey (a
// review-pipeline-failure finding is one-per-commit), so this delegates to
// domain.DeriveFindingID - the single source of truth for finding_id
// derivation - with rule = FailureRule, anchor = gitSHA, empty key.
// repoID and branch are not part of the hash - they are scoped by the
// (finding_id, branch) primary key and the repo_id column - but are accepted
// here so callers pass the full triple and the contract is documented at the
// call site.
func FailureFindingID(repoID, branch, gitSHA string) string {
	return domain.DeriveFindingID(FailureRule, gitSHA, "")
}
