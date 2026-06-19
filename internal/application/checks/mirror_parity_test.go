// SPDX-License-Identifier: AGPL-3.0-only

package checks_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
)

// These tests guard the hand-maintained mirror invariant documented on
// application.CheckRunInput / application.Line: those types are deliberate
// copies of checks.Input / checks.Line, re-declared so the application package
// need not import checks. The copy is correct, but the parity is enforced by
// hand - a field added to checks.Input must be mirrored. A field-signature
// comparison fails compilation-adjacent here the moment the two drift, turning
// a silent latent bug into a red test.
// The Line package qualifier is normalized away so map[string]checks.Line
// and map[string]application.Line compare equal by shape.

func normalizeLineType(s string) string {
	s = strings.ReplaceAll(s, "checks.Line", "Line")
	s = strings.ReplaceAll(s, "application.Line", "Line")
	return s
}

func fieldSignature(t reflect.Type) []string {
	sig := make([]string, 0, t.NumField())
	for f := range t.Fields() {
		sig = append(sig, f.Name+" "+normalizeLineType(f.Type.String()))
	}
	return sig
}

func TestCheckRunInputMirrorsChecksInput(t *testing.T) {
	got := fieldSignature(reflect.TypeFor[application.CheckRunInput]())
	want := fieldSignature(reflect.TypeFor[checks.Input]())
	if !reflect.DeepEqual(got, want) {
		t.Errorf("application.CheckRunInput drifted from checks.Input - keep them in sync (promoter.go vs checks.go)\n  application.CheckRunInput: %v\n  checks.Input:              %v", got, want)
	}
}

func TestApplicationLineMirrorsChecksLine(t *testing.T) {
	got := fieldSignature(reflect.TypeFor[application.Line]())
	want := fieldSignature(reflect.TypeFor[checks.Line]())
	if !reflect.DeepEqual(got, want) {
		t.Errorf("application.Line drifted from checks.Line - keep them in sync\n  application.Line: %v\n  checks.Line:      %v", got, want)
	}
}
