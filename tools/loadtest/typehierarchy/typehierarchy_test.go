// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

// Command typehierarchy is the accuracy oracle for the IMPLEMENTS edges produced
// by promotion (solov2-m5ud.1). It cold-scans this repo in-process through the
// real parser + promotion, then computes ground truth with go/types over the
// SAME files (enumerated via go/packages so both sides see identical input) and
// reports precision / recall of Veska's tree-sitter heuristic against the
// compiler. The design optimizes for PRECISION: a false IMPLEMENTS edge misleads
// an agent worse than a missing one.
//
// Run: make eval-type-hierarchy
package typehierarchy

import (
	"context"
	"database/sql"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

const modulePath = "github.com/whiskeyjimbo/veska"

// pair is a directed type->interface relationship keyed by (relDir, name) on
// both ends, the identity shared by the Veska graph and the go/types oracle.
type pair struct {
	srcDir, srcName string
	dstDir, dstName string
}

func (p pair) String() string {
	return p.srcDir + "." + p.srcName + " -> " + p.dstDir + "." + p.dstName
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller path")
	}
	// tools/loadtest/typehierarchy/<file> -> repo root is three levels up.
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	return root
}

// relDirFromPkgPath maps a go/packages PkgPath to the repo-relative dir used by
// Veska's moduleRelDir (root package -> ""). The second result is false for
// external packages, which Veska does not index.
func relDirFromPkgPath(pkgPath string) (string, bool) {
	if pkgPath == modulePath {
		return "", true
	}
	rest, ok := strings.CutPrefix(pkgPath, modulePath+"/")
	if !ok {
		return "", false
	}
	return rest, true
}

func TestTypeHierarchyOracle(t *testing.T) {
	root := repoRoot(t)
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
		Dir:     root,
		Context: context.Background(),
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}

	// --- Veska side: parse the exact GoFiles and promote them ----------------
	dbPath := filepath.Join(t.TempDir(), "oracle.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: filepath.Join(t.TempDir(), "backups")})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", root, time.Now().UnixMilli(), modulePath,
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	parser := treesitter.NewGoParser()
	var files []application.PromotionFile
	skippedTypeCheck := 0
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			skippedTypeCheck++
			continue // a package that does not type-check is excluded from BOTH sides
		}
		for _, gf := range p.GoFiles {
			src, rerr := os.ReadFile(gf)
			if rerr != nil {
				t.Fatalf("read %s: %v", gf, rerr)
			}
			res, perr := parser.ParseFile(context.Background(), "repo1", gf, src)
			if perr != nil {
				t.Fatalf("parse %s: %v", gf, perr)
			}
			files = append(files, application.PromotionFile{
				Path:            gf,
				Nodes:           res.Nodes,
				Edges:           res.Edges,
				UnresolvedCalls: res.UnresolvedCalls,
				Imports:         res.Imports,
				TypeRels:        res.TypeRels,
			})
		}
	}
	store := sqlite.NewPromotionStore(db, nil)
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "oracle",
		Actor:      domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem},
		PromotedAt: time.Now().UnixMilli(),
		Files:      files,
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	veska := veskaImplements(t, db, root)
	oracle := oracleImplements(pkgs)

	tp, fp, fn := diff(veska, oracle)
	precision := ratio(len(tp), len(tp)+len(fp))
	recall := ratio(len(tp), len(tp)+len(fn))

	t.Logf("=== IMPLEMENTS accuracy (go/types oracle) ===")
	t.Logf("files promoted:     %d (packages skipped for type-check errors: %d)", len(files), skippedTypeCheck)
	t.Logf("veska edges:        %d", len(veska))
	t.Logf("oracle truth:       %d", len(oracle))
	t.Logf("true positives:     %d", len(tp))
	t.Logf("false positives:    %d", len(fp))
	t.Logf("false negatives:    %d", len(fn))
	t.Logf("PRECISION:          %.3f", precision)
	t.Logf("RECALL:             %.3f", recall)
	logSample(t, "false positives (veska says yes, compiler says no)", fp)
	logSample(t, "false negatives (compiler says yes, veska missed)", fn)
}

// veskaImplements reads the IMPLEMENTS edges Veska produced, keyed by the shared
// (relDir, name) identity. The dir is derived from each endpoint's file path.
func veskaImplements(t *testing.T, db *sql.DB, root string) map[pair]bool {
	t.Helper()
	rows, err := db.Query(`
		SELECT s.symbol_path, s.file_path, d.symbol_path, d.file_path
		FROM edges e
		JOIN nodes s ON s.node_id = e.src_node_id AND s.branch = e.branch
		JOIN nodes d ON d.node_id = e.dst_node_id AND d.branch = e.branch
		WHERE e.repo_id = 'repo1' AND e.branch = 'main' AND e.kind = ?`,
		string(domain.EdgeImplements))
	if err != nil {
		t.Fatalf("query implements: %v", err)
	}
	defer rows.Close()
	out := make(map[pair]bool)
	for rows.Next() {
		var sName, sFile, dName, dFile string
		if err := rows.Scan(&sName, &sFile, &dName, &dFile); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[pair{
			srcDir:  fileRelDir(sFile, root),
			srcName: sName,
			dstDir:  fileRelDir(dFile, root),
			dstName: dName,
		}] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// fileRelDir mirrors promotion's moduleRelDir: repo-relative dir, "" at root.
func fileRelDir(path, root string) string {
	p := filepath.ToSlash(path)
	if rest, ok := strings.CutPrefix(p, filepath.ToSlash(root)+"/"); ok {
		p = rest
	}
	dir := filepath.ToSlash(filepath.Dir(p))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// oracleImplements computes ground truth with go/types over the loaded packages,
// restricted to repo-internal type/interface pairs (the only pairs Veska can
// express). Empty interfaces and generic declarations are excluded to match the
// heuristic's MVP scope. A type counts as implementing an interface if either
// the value type or its pointer does (Veska models one node per type).
func oracleImplements(pkgs []*packages.Package) map[pair]bool {
	// Collect repo-internal named types and (separately) interfaces.
	type namedType struct {
		dir  string
		name string
		t    *types.Named
	}
	var allTypes []namedType
	var ifaces []namedType
	seen := make(map[*types.Named]bool)
	collect := func(pkg *packages.Package) {
		dir, ok := relDirFromPkgPath(pkg.PkgPath)
		if !ok || pkg.Types == nil {
			return
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || seen[named] {
				continue
			}
			seen[named] = true
			if named.TypeParams().Len() > 0 {
				continue // generics out of scope
			}
			nt := namedType{dir: dir, name: name, t: named}
			if iface, ok := named.Underlying().(*types.Interface); ok {
				if iface.NumMethods() == 0 {
					continue // empty interface satisfied by everything; skip noise
				}
				ifaces = append(ifaces, nt)
			} else {
				allTypes = append(allTypes, nt)
			}
		}
	}
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			continue
		}
		collect(p)
	}

	out := make(map[pair]bool)
	for _, it := range ifaces {
		iface, _ := it.t.Underlying().(*types.Interface)
		for _, ty := range allTypes {
			if ty.t == it.t {
				continue
			}
			if types.Implements(ty.t, iface) || types.Implements(types.NewPointer(ty.t), iface) {
				out[pair{srcDir: ty.dir, srcName: ty.name, dstDir: it.dir, dstName: it.name}] = true
			}
		}
	}
	return out
}

// diff partitions the two sets into true positives, false positives (veska only)
// and false negatives (oracle only).
func diff(veska, oracle map[pair]bool) (tp, fp, fn []pair) {
	for p := range veska {
		if oracle[p] {
			tp = append(tp, p)
		} else {
			fp = append(fp, p)
		}
	}
	for p := range oracle {
		if !veska[p] {
			fn = append(fn, p)
		}
	}
	return tp, fp, fn
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 1.0
	}
	return float64(num) / float64(den)
}

func logSample(t *testing.T, label string, ps []pair) {
	t.Helper()
	sort.Slice(ps, func(i, j int) bool { return ps[i].String() < ps[j].String() })
	const max = 15
	t.Logf("--- %s (%d) ---", label, len(ps))
	for i, p := range ps {
		if i >= max {
			t.Logf("  ... and %d more", len(ps)-max)
			break
		}
		t.Logf("  %s", p)
	}
}
