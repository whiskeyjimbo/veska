package review

import (
	"crypto/sha256"
	"encoding/hex"
)

// maxReviewAttempts mirrors the queue.Poller retry limit (ADR-S0004: 3
// attempts then state='failed'). The poller increments row.Attempts before
// each Handle call, so the FINAL failing attempt is the one observing
// row.Attempts >= maxReviewAttempts — that is when the Handler emits the
// review-pipeline-failure Finding.
const maxReviewAttempts = 3

// FailureRule is the rule string carried by every review-pipeline-failure
// Finding. It is shared by the Handler (emit), the eng_close_finding handler
// (close-flips-row), and the doctor post-promotion-queue probe (invariant
// check) so all three agree on the same finding identity.
const FailureRule = "review-pipeline-failure"

// FailureFindingID derives the branch-stable finding_id for a
// review-pipeline-failure Finding anchored on a promotion commit.
//
// The Finding anchors its node_id on gitSHA, so this MUST mirror
// domain.NewFinding's finding_id derivation exactly:
//
//	hex(sha256(rule + "\x00" + anchor))[:32]
//
// with rule = FailureRule and anchor = gitSHA. repoID and branch are not part
// of the hash — they are scoped by the (finding_id, branch) primary key and
// the repo_id column — but are accepted here so callers pass the full triple
// and the contract is documented at the call site.
func FailureFindingID(repoID, branch, gitSHA string) string {
	h := sha256.Sum256([]byte(FailureRule + "\x00" + gitSHA))
	return hex.EncodeToString(h[:])[:32]
}
