package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// Review-domain rule strings. Each parsed ReviewFinding becomes a
// domain.Finding carrying one of these rules, selected by ReviewKind. They are
// a distinct rule family from the review-pipeline-failure (FailureRule) and
// budget-exceeded (BudgetRule) findings: those describe the pipeline; these
// describe the code under review.
const (
	// RuleSecurity is the rule carried by every finding produced by the
	// security review kind.
	RuleSecurity = "review-security"
	// RuleContractDrift is the rule carried by every finding produced by the
	// contract-drift review kind.
	RuleContractDrift = "review-contract-drift"
)

// ruleForKind maps a ReviewKind to its domain.Finding rule string. An unknown
// kind yields an error rather than a silent empty rule, so a future ReviewKind
// added without a rule mapping fails loudly.
func ruleForKind(kind ReviewKind) (string, error) {
	switch kind {
	case KindSecurity:
		return RuleSecurity, nil
	case KindContractDrift:
		return RuleContractDrift, nil
	default:
		return "", fmt.Errorf("review: no finding rule for kind %q", kind)
	}
}

// reviewFindingID derives the branch-stable finding_id for a review-produced
// finding. Unlike a pipeline-failure finding (one per commit), a single file
// can yield several review findings under the same rule, so the finding's
// Title is folded into the hash to keep each one distinct:
//
//	hex(sha256(rule + "\x00" + filePath + "\x00" + title))[:32]
//
// Re-reviewing an unchanged file reproduces the same (rule, filePath, title)
// triple and therefore the same id, so FindingStorage.Save is idempotent on
// (finding_id, branch). repoID and branch are not part of the hash — they are
// scoped by the (finding_id, branch) primary key and the repo_id column.
func reviewFindingID(rule, filePath, title string) string {
	h := sha256.Sum256([]byte(rule + "\x00" + filePath + "\x00" + title))
	return hex.EncodeToString(h[:])[:32]
}

// toDomainFinding converts one parsed ReviewFinding into a validated
// domain.Finding anchored on the reviewed file. The finding carries
// source_layer='semantic' and actor_kind='system'; its finding_id is the
// deterministic reviewFindingID so re-review is idempotent.
//
// domain.NewFinding derives a finding_id from rule+anchor alone, which would
// collide for multiple findings sharing a file; the Title-folded id is applied
// after construction to keep each finding distinct.
func toDomainFinding(rf ReviewFinding, repoID, branch, filePath string) (*domain.Finding, error) {
	rule, err := ruleForKind(rf.Kind)
	if err != nil {
		return nil, err
	}
	f, err := domain.NewFinding(
		"", repoID, branch,
		rf.Severity, domain.LayerSemantic,
		rule, rf.Message,
		domain.WithFileAnchor(filePath),
		domain.WithActorKind(domain.ActorKindSystem),
	)
	if err != nil {
		return nil, fmt.Errorf("review: build finding %q: %w", rf.Title, err)
	}
	f.FindingID = reviewFindingID(rule, filePath, rf.Title)
	return f, nil
}
