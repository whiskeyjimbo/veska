package review

import (
	"fmt"
	"slices"
)

// promptSpec binds a review kind to its template file and version. New review
// kinds register by adding a spec here.
type promptSpec struct {
	kind    ReviewKind
	version string
	file    string
}

// registeredPrompts is the full set of review prompts the loader knows. The
// version strings are bumped whenever the corresponding template file's
// wording changes so cached model outputs can be invalidated.
var registeredPrompts = []promptSpec{
	{kind: KindSecurity, version: "security.v1", file: "security.v1.tmpl"},
	{kind: KindContractDrift, version: "contract_drift.v1", file: "contract_drift.v1.tmpl"},
}

// Loader returns versioned review prompts by kind. It is constructed once and
// is safe for concurrent use: after NewLoader the prompt map is read-only.
type Loader struct {
	prompts map[ReviewKind]Prompt
}

// NewLoader constructs a Loader, parsing every registered prompt template. A
// parse failure in a committed template file is a build-time defect and is
// returned as an error.
func NewLoader() (*Loader, error) {
	prompts := make(map[ReviewKind]Prompt, len(registeredPrompts))
	for _, spec := range registeredPrompts {
		p, err := newTemplatedPrompt(spec.kind, spec.version, spec.file)
		if err != nil {
			return nil, err
		}
		prompts[spec.kind] = p
	}
	return &Loader{prompts: prompts}, nil
}

// LoadPrompt returns the versioned Prompt for kind. An unrecognised kind
// returns ErrUnknownKind.
func (l *Loader) LoadPrompt(kind ReviewKind) (Prompt, error) {
	p, ok := l.prompts[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return p, nil
}

// Kinds returns every review kind the loader can serve, in sorted order so
// iteration is deterministic.
func (l *Loader) Kinds() []ReviewKind {
	kinds := make([]ReviewKind, 0, len(l.prompts))
	for k := range l.prompts {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return kinds
}
