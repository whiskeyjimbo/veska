// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package review

import (
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

// toDomainFinding converts one parsed ReviewFinding into a validated
// domain.Finding anchored on the reviewed file. The finding carries
// source_layer='semantic' and actor_kind='system'.
// Unlike a pipeline-failure finding (one per commit), a single file can yield
// several review findings under the same rule, so the finding's Title is
// passed as the WithFindingKey discriminator: domain.NewFinding folds it into
// the finding_id hash so each finding stays distinct. Re-reviewing an
// unchanged file reproduces the same (rule, filePath, title) triple - and
// therefore the same finding_id - so FindingStorage.Save is idempotent on
// (finding_id, branch).
func toDomainFinding(rf ReviewFinding, repoID, branch, filePath string) (*domain.Finding, error) {
	rule, err := ruleForKind(rf.Kind)
	if err != nil {
		return nil, err
	}
	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID:   repoID,
		Branch:   branch,
		Severity: rf.Severity,
		Layer:    domain.LayerSemantic,
		Rule:     rule,
		Message:  rf.Message,
	},
		domain.WithFileAnchor(filePath),
		domain.WithActorKind(domain.ActorKindSystem),
		domain.WithFindingKey(rf.Title),
	)
	if err != nil {
		return nil, fmt.Errorf("review: build finding %q: %w", rf.Title, err)
	}
	return f, nil
}
