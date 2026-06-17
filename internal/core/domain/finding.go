package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

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

// AtLeast returns true when s is at least as severe as other. Unknown severities
// return false conservatively to avoid zero-value aliasing.
func (s Severity) AtLeast(other Severity) bool {
	sRank, sOK := severityOrder[s]
	oRank, oOK := severityOrder[other]
	if !sOK || !oOK {
		return false
	}
	return sRank >= oRank
}

func (s Severity) valid() bool {
	_, ok := severityOrder[s]
	return ok
}

// FindingState is a two-value enum: open or closed.
type FindingState string

const (
	FindingStateOpen   FindingState = "open"
	FindingStateClosed FindingState = "closed"
)

// Finding represents a detected code issue anchored to a symbol or file.
// The FindingID is branch-stable: the same rule, anchor, and optional key
// yield the same identifier across all branches.
type Finding struct {
	FindingID string

	RepoID  string
	Branch  string
	Rule    string
	Message string

	Severity    Severity
	SourceLayer SourceLayer
	State       FindingState

	// Exactly one of NodeID or FilePath must be non-nil.
	NodeID   *string
	FilePath *string

	// Populated only when State is FindingStateClosed.
	ClosedAt     *time.Time
	ClosedReason *string

	ActorID   *string
	ActorKind *ActorKind

	// findingKey is an optional discriminator folded into the FindingID hash.
	// It allows multiple findings to share the same rule and anchor without colliding.
	findingKey string

	// AnchorContentHash is the content hash of the node anchor when the finding was
	// written. It is compared against the node's current hash during revalidation to
	// detect drift and avoid re-firing superseded findings.
	AnchorContentHash *string
}

type FindingOption func(*Finding) error

// WithNodeAnchor sets the node ID anchor for the finding.
func WithNodeAnchor(nodeID string) FindingOption {
	return func(f *Finding) error {
		if nodeID == "" {
			return errors.New("finding: node_id anchor must not be empty")
		}
		f.NodeID = &nodeID
		return nil
	}
}

// WithFileAnchor sets the file path anchor for the finding.
func WithFileAnchor(filePath string) FindingOption {
	return func(f *Finding) error {
		if filePath == "" {
			return errors.New("finding: file_path anchor must not be empty")
		}
		f.FilePath = &filePath
		return nil
	}
}

// WithActorKind sets the actor kind on the finding.
func WithActorKind(ak ActorKind) FindingOption {
	return func(f *Finding) error {
		if _, ok := validActorKinds[ak]; !ok {
			return errors.New("finding: invalid actor_kind")
		}
		f.ActorKind = &ak
		return nil
	}
}

// WithAnchorContentHash sets the content hash of the node anchor captured at creation
// time. Empty hash values are rejected to prevent ambiguity.
func WithAnchorContentHash(hash string) FindingOption {
	return func(f *Finding) error {
		if hash == "" {
			return errors.New("finding: anchor_content_hash must not be empty")
		}
		f.AnchorContentHash = &hash
		return nil
	}
}

// WithFindingKey sets an optional discriminator folded into the FindingID hash to
// allow distinct findings for the same rule and anchor.
func WithFindingKey(key string) FindingOption {
	return func(f *Finding) error {
		f.findingKey = key
		return nil
	}
}

// WithActorID sets the actor ID on the finding.
func WithActorID(id string) FindingOption {
	return func(f *Finding) error {
		if id == "" {
			return errors.New("finding: actor_id must not be empty")
		}
		f.ActorID = &id
		return nil
	}
}

// FindingSpec groups the required fields of a Finding into a struct to prevent
// transposing adjacent same-typed parameters at construction call sites.
type FindingSpec struct {
	RepoID   string
	Branch   string
	Severity Severity
	Layer    SourceLayer
	Rule     string
	Message  string
}

// NewFinding constructs a validated Finding from the specification, enforcing that the
// rule is non-empty, exactly one anchor is provided, and severity and layer are valid.
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

	if f.NodeID == nil && f.FilePath == nil {
		return nil, errors.New("finding: an anchor (node_id or file_path) is required")
	}

	anchor := ""
	if f.NodeID != nil {
		anchor = *f.NodeID
	} else {
		anchor = *f.FilePath
	}
	f.FindingID = DeriveFindingID(spec.Rule, anchor, f.findingKey)

	return f, nil
}

// DeriveFindingID computes the stable FindingID for a Finding. It is the single source
// of truth for ID derivation; any code reconstructing IDs must call this function to
// ensure matching byte-for-byte hash outputs. The repository ID and branch are
// intentionally excluded from the hash to allow findings to be scoped by the
// (FindingID, Branch) primary key.
func DeriveFindingID(rule, anchor, key string) string {
	h := sha256.Sum256([]byte(rule + "\x00" + anchor + "\x00" + key))
	return hex.EncodeToString(h[:])[:32]
}

// Close transitions the finding to the closed state. It requires non-empty
// attribution details and enforces that findings of high severity or above must
// be closed by a human actor.
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
