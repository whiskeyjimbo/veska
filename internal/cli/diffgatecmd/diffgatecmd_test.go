// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	git "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// TestAdaptAbsence is the one behavioral wrinkle of the wiring: the git
// absence sentinel must be translated to diffgate's so deletions are detected.
func TestAdaptAbsence(t *testing.T) {
	// git's "file not at ref" → diffgate's absence sentinel.
	_, err := adaptAbsence(nil, fmt.Errorf("wrapped: %w", git.ErrFileNotAtRef))
	if !errors.Is(err, diffgate.ErrFileAbsentAtRef) {
		t.Fatalf("git.ErrFileNotAtRef should map to diffgate.ErrFileAbsentAtRef, got %v", err)
	}

	// An unrelated error passes through unchanged and is NOT mistaken for absence.
	sentinel := errors.New("git object store unreadable")
	_, err = adaptAbsence(nil, sentinel)
	if errors.Is(err, diffgate.ErrFileAbsentAtRef) {
		t.Fatalf("a generic git error must not be read as absence: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("generic error should pass through, got %v", err)
	}

	// Success passes the bytes through with no error.
	got, err := adaptAbsence([]byte("package a"), nil)
	if err != nil || string(got) != "package a" {
		t.Fatalf("success path = (%q, %v), want (\"package a\", nil)", got, err)
	}
}

func TestRun_RequiresFlags(t *testing.T) {
	if err := Run(t.Context(), Params{}); err == nil {
		t.Fatalf("empty params should error on missing required flags")
	}
}
