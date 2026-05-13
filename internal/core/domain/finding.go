package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// ── SourceLayer ────────────────────────────────────────────────────────────

// SourceLayer is a closed enum of the analysis layers that can produce a Finding.
type SourceLayer string

const (
	LayerStructural SourceLayer = "structural"
	LayerSemantic   SourceLayer = "semantic"
	LayerSecurity   SourceLayer = "security"
	LayerQuality    SourceLayer = "quality"
)

var validSourceLayers = map[SourceLayer]struct{}{
	LayerStructural: {},
	LayerSemantic:   {},
	LayerSecurity:   {},
	LayerQuality:    {},
}

func (l SourceLayer) valid() bool {
	_, ok := validSourceLayers[l]
	return ok
}

// ── Severity ───────────────────────────────────────────────────────────────

// Severity is an ordered closed enum of finding severities.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

var severityOrder = map[Severity]int{
	SeverityInfo:     0,
	SeverityLow:      1,
	SeverityMedium:   2,
	SeverityHigh:     3,
	SeverityCritical: 4,
}

// AtLeast returns true when s is at least as severe as other.
func (s Severity) AtLeast(other Severity) bool {
	return severityOrder[s] >= severityOrder[other]
}

func (s Severity) valid() bool {
	_, ok := severityOrder[s]
	return ok
}

// ── FindingState ───────────────────────────────────────────────────────────

// FindingState is a two-value enum: open or closed.
type FindingState string

const (
	FindingStateOpen   FindingState = "open"
	FindingStateClosed FindingState = "closed"
)

// ── ActorKind ──────────────────────────────────────────────────────────────

// ActorKind distinguishes who or what performed an action.
type ActorKind string

const (
	ActorKindHuman  ActorKind = "human"
	ActorKindAgent  ActorKind = "agent"
	ActorKindSystem ActorKind = "system"
)

var validActorKinds = map[ActorKind]struct{}{
	ActorKindHuman:  {},
	ActorKindAgent:  {},
	ActorKindSystem: {},
}

// ── Finding ────────────────────────────────────────────────────────────────

// Finding represents a detected code issue anchored to a symbol or file.
// The finding_id is branch-stable: same rule + anchor → same finding_id on every branch.
type Finding struct {
	// Per-row primary key (ULID).
	ID string
	// Branch-stable identifier: hex(sha256(rule+"\x00"+anchor))[:32].
	FindingID string

	RepoID  string
	Branch  string
	Rule    string
	Message string

	Severity    Severity
	SourceLayer SourceLayer
	State       FindingState

	// Anchor: exactly one of NodeID or FilePath is non-nil.
	NodeID   *string
	FilePath *string

	// Set iff State == FindingStateClosed.
	ClosedAt     *time.Time
	ClosedReason *string

	// Optional actor metadata.
	ActorID   *string
	ActorKind *ActorKind
}

// FindingOption is a functional option for NewFinding.
type FindingOption func(*Finding) error

// WithNodeAnchor sets the node_id anchor for the finding.
func WithNodeAnchor(nodeID string) FindingOption {
	return func(f *Finding) error {
		if nodeID == "" {
			return errors.New("finding: node_id anchor must not be empty")
		}
		f.NodeID = &nodeID
		return nil
	}
}

// WithFileAnchor sets the file_path anchor for the finding.
func WithFileAnchor(filePath string) FindingOption {
	return func(f *Finding) error {
		if filePath == "" {
			return errors.New("finding: file_path anchor must not be empty")
		}
		f.FilePath = &filePath
		return nil
	}
}

// WithActorKind sets the actor_kind on the finding.
func WithActorKind(ak ActorKind) FindingOption {
	return func(f *Finding) error {
		if _, ok := validActorKinds[ak]; !ok {
			return errors.New("finding: invalid actor_kind")
		}
		f.ActorKind = &ak
		return nil
	}
}

// WithActorID sets the actor_id on the finding.
func WithActorID(id string) FindingOption {
	return func(f *Finding) error {
		f.ActorID = &id
		return nil
	}
}

// NewFinding constructs a validated Finding. The finding_id is computed from
// rule and anchor; it is not accepted as a parameter.
//
// Invariants enforced:
//  1. rule non-empty.
//  2. Exactly one anchor (node_id or file_path) provided.
//  3. severity and source_layer must be valid enum values.
//  4. State defaults to open; closed_at and closed_reason are nil.
func NewFinding(
	id, repoID, branch string,
	severity Severity,
	layer SourceLayer,
	rule, message string,
	opts ...FindingOption,
) (*Finding, error) {
	if rule == "" {
		return nil, errors.New("finding: rule must not be empty")
	}
	if !severity.valid() {
		return nil, errors.New("finding: invalid severity")
	}
	if !layer.valid() {
		return nil, errors.New("finding: invalid source_layer")
	}

	f := &Finding{
		ID:          id,
		RepoID:      repoID,
		Branch:      branch,
		Severity:    severity,
		SourceLayer: layer,
		Rule:        rule,
		Message:     message,
		State:       FindingStateOpen,
	}

	for _, opt := range opts {
		if err := opt(f); err != nil {
			return nil, err
		}
	}

	// Invariant: exactly one anchor required.
	if f.NodeID == nil && f.FilePath == nil {
		return nil, errors.New("finding: an anchor (node_id or file_path) is required")
	}

	// Compute branch-stable finding_id.
	anchor := ""
	if f.NodeID != nil {
		anchor = *f.NodeID
	} else {
		anchor = *f.FilePath
	}
	h := sha256.Sum256([]byte(rule + "\x00" + anchor))
	f.FindingID = hex.EncodeToString(h[:])[:32]

	return f, nil
}

// Close transitions the finding to the closed state.
//
// Invariant: severity >= high requires actorKind == human.
func (f *Finding) Close(reason, actorKindStr, actorID string, now time.Time) error {
	if f.State == FindingStateClosed {
		return errors.New("finding: already closed")
	}
	ak := ActorKind(actorKindStr)
	if f.Severity.AtLeast(SeverityHigh) && ak != ActorKindHuman {
		return errors.New("finding: severity >= high requires a human actor to close")
	}
	f.State = FindingStateClosed
	f.ClosedAt = &now
	f.ClosedReason = &reason
	f.ActorKind = &ak
	f.ActorID = &actorID
	return nil
}
