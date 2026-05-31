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

// ── Finding ────────────────────────────────────────────────────────────────

// Finding represents a detected code issue anchored to a symbol or file.
// The finding_id is branch-stable: same rule + anchor (+ optional discriminator
// key) → same finding_id on every branch.
type Finding struct {
	// Branch-stable identifier and primary-key component (with Branch):
	// hex(sha256(rule+"\x00"+anchor+"\x00"+key))[:32].
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

	// findingKey is an optional discriminator folded into the finding_id hash.
	// It lets a caller emit several findings sharing the same (rule, anchor)
	// — e.g. multiple review-code findings in one file — without their
	// finding_ids colliding. It defaults to "" and is not persisted.
	findingKey string

	// AnchorContentHash is the content_hash of the node anchor at the moment
	// the finding was written. It is populated only when the finding anchors
	// on a node whose content_hash is known to the producing check (dead-code,
	// contract-drift, auto-link). File-anchored findings (parse-failure) leave
	// it nil — the file as a whole has no per-symbol hash.
	//
	// The revalidation sweep (m3.05.2) compares this against the node's
	// current content_hash to detect drift: a finding whose anchor has moved
	// on is superseded rather than re-fired.
	AnchorContentHash *string
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

// WithAnchorContentHash sets the content_hash of the node anchor captured at
// finding-write time. An empty hash is rejected so callers cannot silently
// confuse "anchor has no hash" (nil) with "anchor's hash is the empty string"
// — mirrors the empty-anchor validation on WithNodeAnchor / WithFileAnchor.
func WithAnchorContentHash(hash string) FindingOption {
	return func(f *Finding) error {
		if hash == "" {
			return errors.New("finding: anchor_content_hash must not be empty")
		}
		f.AnchorContentHash = &hash
		return nil
	}
}

// WithFindingKey sets an optional discriminator folded into the finding_id
// hash. Two findings with identical (rule, anchor) but different keys get
// distinct finding_ids; an unset key (the default "") reproduces the plain
// rule+anchor derivation. Use it when one anchor can carry several distinct
// findings under the same rule (e.g. multiple review-code findings per file).
func WithFindingKey(key string) FindingOption {
	return func(f *Finding) error {
		f.findingKey = key
		return nil
	}
}

// WithActorID sets the actor_id on the finding.
func WithActorID(id string) FindingOption {
	return func(f *Finding) error {
		if id == "" {
			return errors.New("finding: actor_id must not be empty")
		}
		f.ActorID = &id
		return nil
	}
}

// FindingSpec carries the required fields of a Finding. It groups the
// constructor's positional arguments into a named struct so adjacent
// same-typed fields (RepoID/Branch, Rule/Message) cannot be transposed at a
// call site. Optional fields (anchors, actor, content hash, discriminator
// key) are still supplied via FindingOption.
type FindingSpec struct {
	RepoID   string
	Branch   string
	Severity Severity
	Layer    SourceLayer
	Rule     string
	Message  string
}

// NewFinding constructs a validated Finding from spec. The finding_id is
// computed from spec.Rule, the anchor, and an optional discriminator key (see
// WithFindingKey); it is never accepted as a parameter.
//
// Invariants enforced:
//  1. spec.Rule non-empty.
//  2. Exactly one anchor (node_id or file_path) provided.
//  3. spec.Severity and spec.Layer must be valid enum values.
//  4. State defaults to open; closed_at and closed_reason are nil.
func NewFinding(spec FindingSpec, opts ...FindingOption) (*Finding, error) {
	if spec.Rule == "" {
		return nil, errors.New("finding: rule must not be empty")
	}
	if !spec.Severity.valid() {
		return nil, errors.New("finding: invalid severity")
	}
	if !spec.Layer.valid() {
		return nil, errors.New("finding: invalid source_layer")
	}

	f := &Finding{
		RepoID:      spec.RepoID,
		Branch:      spec.Branch,
		Severity:    spec.Severity,
		SourceLayer: spec.Layer,
		Rule:        spec.Rule,
		Message:     spec.Message,
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
	f.FindingID = DeriveFindingID(spec.Rule, anchor, f.findingKey)

	return f, nil
}

// DeriveFindingID computes the branch-stable finding_id for a Finding:
// hex(sha256(rule + "\x00" + anchor + "\x00" + key))[:32].
//
// It is the single source of truth for finding_id derivation. NewFinding uses
// it internally; any code that must reconstruct a finding_id without a Finding
// in hand (e.g. a doctor probe correlating a queue row to its companion
// finding) MUST call this rather than re-implementing the hash — the two must
// stay byte-identical or correlation silently breaks.
//
// anchor is the finding's node_id or file_path. key is the optional
// discriminator set via WithFindingKey ("" when the finding is one-per-anchor).
// repoID and branch are intentionally NOT part of the hash — a finding is
// scoped by the (finding_id, branch) primary key and the repo_id column.
func DeriveFindingID(rule, anchor, key string) string {
	h := sha256.Sum256([]byte(rule + "\x00" + anchor + "\x00" + key))
	return hex.EncodeToString(h[:])[:32]
}

// Close transitions the finding to the closed state.
//
// Invariants:
//   - reason and actorID must be non-empty (mirrors NewSuppression, so the
//     close path cannot silently blank attribution or audit reason).
//   - actorKind must be a recognised ActorKind (same check NewActor and
//     WithActorKind enforce — the close path must not be the one place an
//     unvalidated kind slips onto a Finding).
//   - severity >= high requires actorKind == human.
func (f *Finding) Close(reason string, actorKind ActorKind, actorID string, now time.Time) error {
	if f.State == FindingStateClosed {
		return errors.New("finding: already closed")
	}
	if reason == "" {
		return errors.New("finding: reason must not be empty")
	}
	if actorID == "" {
		return errors.New("finding: actor_id must not be empty")
	}
	if _, ok := validActorKinds[actorKind]; !ok {
		return errors.New("finding: invalid actor_kind")
	}
	if f.Severity.AtLeast(SeverityHigh) && actorKind != ActorKindHuman {
		return errors.New("finding: severity >= high requires a human actor to close")
	}
	f.State = FindingStateClosed
	f.ClosedAt = &now
	f.ClosedReason = &reason
	f.ActorKind = &actorKind
	f.ActorID = &actorID
	return nil
}
