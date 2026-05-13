// Package layercheck implements an import-graph layer analyser that enforces
// architectural boundaries between core, application, and infrastructure layers.
//
// Forbidden import directions:
//
//	internal/core/**         → internal/application/**
//	internal/core/**         → internal/infrastructure/**
//	internal/application/**  → internal/infrastructure/**
//
// Allowed exceptions (application may cross only to ports/domain):
//
//	internal/application/** → internal/core/ports/**   (allowed)
//	internal/application/** → internal/core/domain/**  (allowed)
package layercheck

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Violation describes a single forbidden import.
type Violation struct {
	Importer string
	Imported string
	Rule     string
}

func (v Violation) String() string {
	return fmt.Sprintf("LAYER VIOLATION [%s]: %s → %s", v.Rule, v.Importer, v.Imported)
}

// IsViolation returns true when importing `imported` from `importer` breaches a
// layer rule.  Both paths are expected to be fully-qualified Go package import
// paths (e.g. "github.com/foo/bar/internal/core/domain").
func IsViolation(importer, imported string) bool {
	_, violated := checkViolation(importer, imported)
	return violated
}

// checkViolation returns the rule name and true when a violation is detected.
func checkViolation(importer, imported string) (rule string, violated bool) {
	inCore := containsSegment(importer, "internal/core")
	inApp := containsSegment(importer, "internal/application")

	switch {
	case inCore && containsSegment(imported, "internal/application"):
		return "core→application", true

	case inCore && containsSegment(imported, "internal/infrastructure"):
		return "core→infrastructure", true

	case inApp && containsSegment(imported, "internal/infrastructure"):
		return "application→infrastructure", true
	}

	return "", false
}

// containsSegment reports whether path contains the given slash-separated
// segment as a complete sub-path (not just a substring).
// It matches both "…/segment/…" and "…/segment" (at the end).
func containsSegment(path, segment string) bool {
	// Normalise to forward slashes (handles Windows if ever needed).
	path = filepath.ToSlash(path)
	return strings.Contains(path, "/"+segment+"/") ||
		strings.HasSuffix(path, "/"+segment)
}

// CheckDir loads the package graph rooted at dir and returns all layer violations
// found in packages under internal/.
func CheckDir(dir string) ([]Violation, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedImports | packages.NeedDeps,
		Dir:  dir,
		// Skip test files; we only care about production import graphs.
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, "./internal/...")
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	var violations []Violation
	seen := make(map[string]bool)

	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			// Surface load errors but don't abort — partial results are still useful.
			for _, e := range pkg.Errors {
				fmt.Printf("warning: package %s: %v\n", pkg.PkgPath, e)
			}
		}

		for importedPath := range pkg.Imports {
			key := pkg.PkgPath + "→" + importedPath
			if seen[key] {
				continue
			}
			seen[key] = true

			rule, violated := checkViolation(pkg.PkgPath, importedPath)
			if violated {
				violations = append(violations, Violation{
					Importer: pkg.PkgPath,
					Imported: importedPath,
					Rule:     rule,
				})
			}
		}
	}

	return violations, nil
}
