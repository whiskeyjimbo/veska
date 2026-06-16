// Package extindex indexes a registered repo's vendored Go module
// sources into the graph as external nodes.
// Why: the multi-repo wedge promises "your agent knows
// what calls what across all your repos." Today that fails the moment
// a call crosses into a third-party module that isn't itself
// registered: cross_repo_edge_stubs have no destination to bind to.
// Indexing vendor/ adds those destination nodes so eng_find_symbol,
// eng_get_call_chain, eng_get_blast_radius see into the imported code
// instead of dead-ending at the module boundary.
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

// ExternalRepoUpserter inserts a synthetic repo row for an indexed
// vendor module. The row's module_path lets the
// existing cross_repo_edge_stubs resolver find vendored destinations
// for CALLS edges — without it, a stub from myapp.Run targeting
// greetlib.New would dead-end at the import boundary.
// Implementations should:
//
//	INSERT … ON CONFLICT DO NOTHING (idempotent re-index)
//	use the supplied synthetic repo_id (caller decides the format;
//	  today's convention is "ext:<module-path>" — no version yet)
type ExternalRepoUpserter interface {
	UpsertExternalRepo(ctx context.Context, repoID, rootPath, modulePath, branch string) error
}

// SyntheticRepoIDPrefix marks a repos row that represents an
// indexed external module rather than a user-registered git repo.
// CLI consumers (eng_list_repos) filter on this prefix by default.
const SyntheticRepoIDPrefix = "ext:"

// SyntheticRepoID returns the synthetic repo_id for a vendor-indexed
// module. Phase 1 ignores version (vendor/ is checkpoint-locked);
// phase 2 with $GOMODCACHE will extend this to "ext:<module>@<version>".
func SyntheticRepoID(modulePath string) string {
	return SyntheticRepoIDPrefix + modulePath
}

// Result summarises one IndexVendorModule call. Callers print it to
// stderr / stdout so the user sees what was indexed.
type Result struct {
	ModulePath string
	Files      int
	Nodes      int
	Skipped    int // go files the parser couldn't extract symbols from
}

// Service is the application-level facade. It is stateless; the same
// instance is safe for concurrent callers.
type Service struct {
	parser   ports.CodeParser
	saver    ExternalNodeSaver
	upserter ExternalRepoUpserter
}

// NewService constructs a Service. parser + saver are required;
// upserter is optional — when nil, indexed nodes still get written
// against the importing repo's repo_id (phase 1 fallback) and
// cross-repo CALLS edges through them do NOT resolve. With an
// upserter wired, the indexer creates a synthetic repo per module
// and writes nodes against it, which closes the resolver loop
func NewService(parser ports.CodeParser, saver ExternalNodeSaver, opts ...Option) (*Service, error) {
	if parser == nil {
		return nil, fmt.Errorf("extindex.NewService: parser is nil: %w", ErrMissingDependency)
	}
	if saver == nil {
		return nil, fmt.Errorf("extindex.NewService: saver is nil: %w", ErrMissingDependency)
	}
	s := &Service{parser: parser, saver: saver}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Option configures a Service at construction time.
type Option func(*Service)

// WithExternalRepoUpserter wires the synthetic-repo writer that
// closes the cross-repo CALLS resolution loop. Without
// this option indexed nodes still appear in eng_find_symbol but
// cross_repo_edge_stubs targeting them won't bind.
func WithExternalRepoUpserter(u ExternalRepoUpserter) Option {
	return func(s *Service) { s.upserter = u }
}

// IndexVendorModule walks <repoRoot>/vendor/<modulePath> for.go
// files, parses each via the injected CodeParser, and persists the
// resulting nodes with external=1 against (repoID, branch). Returns
// ErrModuleNotVendored when the path doesn't exist — callers can
// distinguish "module not vendored" from a genuine I/O error.
// Test files (`_test.go`) are skipped: they aren't part of the
// shipped API and would balloon the external-node count with stub
// types. Subdirectories of vendor/<modulePath> ARE walked so
// multi-package modules (e.g. cobra has cobra/doc, cobra/cmd,.)
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

	// when an upserter is wired, write nodes under a
	// synthetic per-module repo row so cross_repo_edge_stubs can bind
	// against module_path. Without it (older callers), fall back to
	// the importing repo's repo_id — find_symbol still surfaces the
	// nodes but CALLS edges through them stay unresolved.
	nodeRepoID := repoID
	nodeBranch := branch
	if s.upserter != nil {
		nodeRepoID = SyntheticRepoID(modulePath)
		nodeBranch = "main"
		if err := s.upserter.UpsertExternalRepo(ctx, nodeRepoID, modDir, modulePath, nodeBranch); err != nil {
			return Result{}, fmt.Errorf("extindex: upsert synthetic repo for %s: %w", modulePath, err)
		}
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
		// Parser sees the synthetic repo_id when upserter is wired
		// (yr56) so node_id hashes are deterministic per module
		// re-indexing the same vendor tree produces the same IDs
		// regardless of which importing repo triggered it.
		pr, perr := s.parser.ParseFile(ctx, nodeRepoID, path, src)
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
			if err := s.saver.SaveExternalNode(ctx, nodeRepoID, nodeBranch, n); err != nil {
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
