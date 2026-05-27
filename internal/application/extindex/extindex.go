// Package extindex indexes a registered repo's vendored Go module
// sources into the graph as external nodes (solov2-bchl phase 1).
//
// Why: the multi-repo wedge (solov2-71xq) promises "your agent knows
// what calls what across all your repos." Today that fails the moment
// a call crosses into a third-party module that isn't itself
// registered: cross_repo_edge_stubs have no destination to bind to.
// Indexing vendor/ adds those destination nodes so eng_find_symbol,
// eng_get_call_chain, eng_get_blast_radius see into the imported code
// instead of dead-ending at the module boundary.
//
// Phase 1 scope: vendor/ only (no $GOMODCACHE yet), manual trigger via
// `veska deps index <module>`, no embeddings, no auto-rescan on
// go.sum change. Each phase landing widens the scope without changing
// the storage contract (external=1 on the nodes table).
package extindex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrMissingDependency is returned by NewService when a required
// collaborator is nil. Mirrors the sentinel pattern used by sibling
// services.
var ErrMissingDependency = errors.New("missing dependency")

// ErrModuleNotVendored is returned by IndexVendorModule when the
// importing repo has no vendor/<module> directory. Callers may decide
// whether that's a hard failure or a quiet skip (the CLI surfaces it
// as a clear message; auto-trigger callers may want to silently move
// on to the next dep).
var ErrModuleNotVendored = errors.New("module not vendored")

// ExternalNodeSaver is the narrow write port the indexer consumes.
// *sqlite.GraphRepo's SaveExternalNode satisfies it — the indirection
// keeps the application layer decoupled from the sqlite adapter and
// lets tests stub it without spinning up a DB.
type ExternalNodeSaver interface {
	SaveExternalNode(ctx context.Context, repoID, branch string, n *domain.Node) error
}

// Result summarises one IndexVendorModule call. Callers print it to
// stderr / stdout so the user sees what was indexed.
type Result struct {
	ModulePath string
	Files      int
	Nodes      int
	Skipped    int // .go files the parser couldn't extract symbols from
}

// Service is the application-level facade. It is stateless; the same
// instance is safe for concurrent callers.
type Service struct {
	parser ports.CodeParser
	saver  ExternalNodeSaver
}

// NewService constructs a Service. Both deps required; mirrors the
// sentinel-wrap pattern other application services use.
func NewService(parser ports.CodeParser, saver ExternalNodeSaver) (*Service, error) {
	if parser == nil {
		return nil, fmt.Errorf("extindex.NewService: parser is nil: %w", ErrMissingDependency)
	}
	if saver == nil {
		return nil, fmt.Errorf("extindex.NewService: saver is nil: %w", ErrMissingDependency)
	}
	return &Service{parser: parser, saver: saver}, nil
}

// IndexVendorModule walks <repoRoot>/vendor/<modulePath> for .go
// files, parses each via the injected CodeParser, and persists the
// resulting nodes with external=1 against (repoID, branch). Returns
// ErrModuleNotVendored when the path doesn't exist — callers can
// distinguish "module not vendored" from a genuine I/O error.
//
// Test files (`_test.go`) are skipped: they aren't part of the
// shipped API and would balloon the external-node count with stub
// types. Subdirectories of vendor/<modulePath> ARE walked so
// multi-package modules (e.g. cobra has cobra/doc, cobra/cmd, ...)
// are indexed too.
func (s *Service) IndexVendorModule(ctx context.Context, repoID, branch, repoRoot, modulePath string) (Result, error) {
	modDir := filepath.Join(repoRoot, "vendor", modulePath)
	info, err := os.Stat(modDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("%w: %s", ErrModuleNotVendored, modDir)
		}
		return Result{}, fmt.Errorf("extindex: stat %s: %w", modDir, err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("extindex: %s is not a directory", modDir)
	}

	res := Result{ModulePath: modulePath}
	walkErr := filepath.WalkDir(modDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			res.Skipped++
			return nil
		}
		// repoID for the parser is the importing repo — keeps the
		// repo_id constraint on the node row pointing at the parent
		// repo, so a `veska repo remove <repo>` cascades cleanly.
		pr, perr := s.parser.ParseFile(ctx, repoID, path, src)
		if perr != nil {
			res.Skipped++
			return nil
		}
		if pr == nil {
			res.Skipped++
			return nil
		}
		res.Files++
		for _, n := range pr.Nodes {
			if n == nil {
				continue
			}
			if err := s.saver.SaveExternalNode(ctx, repoID, branch, n); err != nil {
				return fmt.Errorf("extindex: save %s: %w", n.ID, err)
			}
			res.Nodes++
		}
		return nil
	})
	if walkErr != nil {
		return res, walkErr
	}
	return res, nil
}
