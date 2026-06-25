// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// resolveTypeRelations materializes EMBEDS edges from parsed embed facts and then
// IMPLEMENTS edges from Go method-set satisfaction. EMBEDS must run first: a type
// can satisfy an interface through methods promoted from an embedded type, so the
// embed graph has to exist before satisfaction is computed.
func (p *promotion) resolveTypeRelations(ctx context.Context) error {
	if err := p.resolveEmbeds(ctx); err != nil {
		return err
	}
	return p.resolveImplements(ctx)
}

// resolveEmbeds binds each parsed embed fact to its target type node and inserts
// an EMBEDS edge. Targets outside the indexed graph (e.g. stdlib *log.Logger) are
// skipped: there is no node to point at and their methods are not visible anyway.
func (p *promotion) resolveEmbeds(ctx context.Context) error {
	byDir := buildPackageSymbolMap(p.batch)
	byModDir := buildModuleRelSymbolMap(p.batch, p.rootPath)
	for _, file := range p.batch.Files {
		for _, tr := range file.TypeRels {
			if tr.Kind != domain.EdgeEmbeds {
				continue
			}
			targetID, ok, err := p.resolveTypeTarget(ctx, byDir, byModDir, file, tr)
			if err != nil {
				return err
			}
			if !ok || tr.SrcID == targetID {
				continue
			}
			opts := []domain.EdgeOption{domain.WithConfidence(domain.Definite)}
			if tr.SrcLine > 0 {
				opts = append(opts, domain.WithSourceLine(tr.SrcLine))
			}
			e, err := domain.NewEdge(domain.EdgeSpec{Src: tr.SrcID, Tgt: targetID, Kind: domain.EdgeEmbeds}, opts...)
			if err != nil {
				continue
			}
			if err := p.insertEdge(ctx, e); err != nil {
				return fmt.Errorf("promoter: insert EMBEDS edge %q: %w", e.ID, err)
			}
		}
	}
	return nil
}

// resolveTypeTarget resolves an embed target name to a promoted type node, using
// the batch first and the promoted graph as a fallback so incremental commits
// bind to unchanged sibling files. Cross-package targets are resolved via the
// file's imports when they live in the same module; external targets return
// not-found.
func (p *promotion) resolveTypeTarget(ctx context.Context, byDir, byModDir map[string]map[string]domain.NodeID, file application.PromotionFile, tr domain.UnresolvedTypeRel) (domain.NodeID, bool, error) {
	if tr.PkgQualifier == "" {
		if id, ok := byDir[filepath.Dir(file.Path)][tr.TargetName]; ok {
			return id, true, nil
		}
		scope := promotedScope{repoID: p.repoID, branch: p.branch, root: p.rootPath, relDir: moduleRelDir(file.Path, p.rootPath)}
		return lookupPromotedSymbolDir(ctx, p.tx, scope, tr.TargetName)
	}
	importPath, ok := file.Imports[tr.PkgQualifier]
	if !ok || p.modulePath == "" {
		return "", false, nil
	}
	relDir, inModule := modulePackageDir(p.modulePath, importPath)
	if !inModule {
		return "", false, nil // external package; target not indexed here
	}
	if id, ok := byModDir[relDir][tr.TargetName]; ok {
		return id, true, nil
	}
	scope := promotedScope{repoID: p.repoID, branch: p.branch, root: p.rootPath, relDir: relDir}
	return lookupPromotedSymbolDir(ctx, p.tx, scope, tr.TargetName)
}

// --- IMPLEMENTS resolution ---------------------------------------------------

// methodSig is a normalized method for set comparison. norm is the
// name-independent (params)->(results) signature with package qualifiers reduced
// to their base type name; exact is false when the signature could not be parsed
// and only the arity is trusted; qualified records that a package-qualified type
// was reduced (so a match is "likely" rather than "definite").
type methodSig struct {
	name      string
	norm      string
	arity     string
	exact     bool
	qualified bool
	pointer   bool // pointer receiver (concrete methods only)
}

// typeDecl is a promoted struct/interface/type node with the data needed for
// satisfaction.
type typeDecl struct {
	id      domain.NodeID
	name    string
	dir     string
	iface   bool
	generic bool
	inBatch bool
}

// resolveImplements computes Go interface satisfaction over the promoted graph
// and inserts IMPLEMENTS edges (type -> interface). It loads the type/method
// picture once, then only evaluates pairs where the type or the interface was
// touched by this batch, so unchanged-vs-unchanged pairs are not recomputed.
func (p *promotion) resolveImplements(ctx context.Context) error {
	methodsByOwner, err := p.loadMethodsByOwner(ctx)
	if err != nil {
		return err
	}
	decls, err := p.loadTypeDecls(ctx)
	if err != nil {
		return err
	}
	embeds, err := p.loadEmbedClosure(ctx)
	if err != nil {
		return err
	}
	byID := make(map[domain.NodeID]*typeDecl, len(decls))
	for i := range decls {
		byID[decls[i].id] = &decls[i]
	}

	// Precompute required sets (interfaces) and method sets (types), expanding
	// embedded methods transitively.
	requiredOf := func(d *typeDecl) []methodSig {
		return collectMethods(d, byID, methodsByOwner, embeds)
	}
	methodSetOf := func(d *typeDecl) map[string][]methodSig {
		ms := collectMethods(d, byID, methodsByOwner, embeds)
		out := make(map[string][]methodSig, len(ms))
		for _, m := range ms {
			out[m.name] = append(out[m.name], m)
		}
		return out
	}

	var ifaces, types []*typeDecl
	for i := range decls {
		d := &decls[i]
		if d.generic {
			continue // generics out of scope; do not emit misleading edges
		}
		if d.iface {
			ifaces = append(ifaces, d)
		} else {
			types = append(types, d)
		}
	}

	for _, iface := range ifaces {
		required := requiredOf(iface)
		if len(required) == 0 {
			continue // empty interface is satisfied by everything; skip the noise
		}
		for _, typ := range types {
			if !iface.inBatch && !typ.inBatch {
				continue // neither side changed this batch
			}
			have := methodSetOf(typ)
			ok, conf := satisfies(required, have)
			if !ok {
				continue
			}
			e, err := domain.NewEdge(domain.EdgeSpec{Src: typ.id, Tgt: iface.id, Kind: domain.EdgeImplements}, domain.WithConfidence(conf))
			if err != nil {
				continue
			}
			if err := p.insertEdge(ctx, e); err != nil {
				return fmt.Errorf("promoter: insert IMPLEMENTS edge %q: %w", e.ID, err)
			}
		}
	}
	return nil
}

// satisfies reports whether a type whose method set is `have` satisfies the
// required interface method set, and at what confidence. A required method must
// be matched by name AND normalized signature; an arity-only match (when a
// signature could not be parsed) is accepted but downgrades confidence to
// Probable. A package-qualified type reduction downgrades to Strong.
func satisfies(required []methodSig, have map[string][]methodSig) (bool, domain.Confidence) {
	conf := domain.Definite
	for _, req := range required {
		cands := have[req.name]
		if len(cands) == 0 {
			return false, 0
		}
		matched := false
		for _, c := range cands {
			switch {
			case req.exact && c.exact && req.norm == c.norm:
				matched = true
				if req.qualified || c.qualified {
					conf = minConf(conf, domain.Strong)
				}
			case (!req.exact || !c.exact) && req.arity == c.arity:
				// Fell back to arity because a signature would not parse.
				matched = true
				conf = minConf(conf, domain.Probable)
			}
			if matched {
				break
			}
		}
		if !matched {
			return false, 0
		}
	}
	return true, conf
}

func minConf(a, b domain.Confidence) domain.Confidence {
	if b < a {
		return b
	}
	return a
}

// collectMethods returns the method set of a declaration, following EMBEDS edges
// transitively to promote embedded methods. The same traversal serves both an
// interface's required set (its members + embedded-interface members) and a
// concrete type's method set (its methods + methods promoted from embedded
// types), so callers don't distinguish the two.
func collectMethods(d *typeDecl, byID map[domain.NodeID]*typeDecl, methodsByOwner map[string][]methodSig, embeds map[domain.NodeID][]domain.NodeID) []methodSig {
	seen := make(map[domain.NodeID]bool)
	var out []methodSig
	var walk func(cur *typeDecl)
	walk = func(cur *typeDecl) {
		if cur == nil || seen[cur.id] {
			return
		}
		seen[cur.id] = true
		out = append(out, methodsByOwner[ownerKey(cur.dir, cur.name)]...)
		for _, tgt := range embeds[cur.id] {
			walk(byID[tgt])
		}
	}
	walk(d)
	return out
}

func ownerKey(dir, name string) string { return dir + "\x00" + name }

// loadMethodsByOwner groups promoted method nodes by (package dir, receiver/iface
// type name). symbol_path is "Owner.Method"; the dir comes from the file path.
func (p *promotion) loadMethodsByOwner(ctx context.Context) (map[string][]methodSig, error) {
	rows, err := p.tx.QueryContext(ctx,
		`SELECT symbol_path, file_path, COALESCE(signature,''), COALESCE(snippet,'')
		   FROM nodes WHERE repo_id = ? AND branch = ? AND kind = 'method'`,
		p.repoID, p.branch)
	if err != nil {
		return nil, fmt.Errorf("promoter: load methods: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]methodSig)
	for rows.Next() {
		var symbolPath, filePath, signature, snippet string
		if err := rows.Scan(&symbolPath, &filePath, &signature, &snippet); err != nil {
			return nil, fmt.Errorf("promoter: scan method: %w", err)
		}
		dot := strings.LastIndex(symbolPath, ".")
		if dot <= 0 {
			continue
		}
		owner := symbolPath[:dot]
		name := symbolPath[dot+1:]
		dir := moduleRelDir(filePath, p.rootPath)
		out[ownerKey(dir, owner)] = append(out[ownerKey(dir, owner)], parseMethodSig(name, signature, snippet))
	}
	return out, rows.Err()
}

// loadTypeDecls loads struct/type/interface nodes with the data satisfaction
// needs. Generic declarations are flagged so they can be skipped.
func (p *promotion) loadTypeDecls(ctx context.Context) ([]typeDecl, error) {
	batchDirs := p.batchTypeNodeIDs()
	rows, err := p.tx.QueryContext(ctx,
		`SELECT node_id, symbol_path, file_path, kind, COALESCE(snippet,'')
		   FROM nodes WHERE repo_id = ? AND branch = ? AND kind IN ('interface','struct','type')`,
		p.repoID, p.branch)
	if err != nil {
		return nil, fmt.Errorf("promoter: load type decls: %w", err)
	}
	defer rows.Close()
	var out []typeDecl
	for rows.Next() {
		var id, name, filePath, kind, snippet string
		if err := rows.Scan(&id, &name, &filePath, &kind, &snippet); err != nil {
			return nil, fmt.Errorf("promoter: scan type decl: %w", err)
		}
		out = append(out, typeDecl{
			id:      domain.NodeID(id),
			name:    name,
			dir:     moduleRelDir(filePath, p.rootPath),
			iface:   kind == "interface",
			generic: isGenericDecl(snippet, name),
			inBatch: batchDirs[domain.NodeID(id)],
		})
	}
	return out, rows.Err()
}

// batchTypeNodeIDs returns the node IDs of type/interface declarations promoted
// in this batch, used to scope IMPLEMENTS recomputation to changed declarations.
func (p *promotion) batchTypeNodeIDs() map[domain.NodeID]bool {
	out := make(map[domain.NodeID]bool)
	for _, file := range p.batch.Files {
		for _, n := range file.Nodes {
			if n == nil {
				continue
			}
			switch n.Kind {
			case domain.KindInterface, domain.KindStruct, domain.KindType:
				out[n.ID] = true
			}
		}
	}
	return out
}

// loadEmbedClosure returns the EMBEDS adjacency (embedder -> embedded targets)
// for the repo/branch, used to promote embedded methods during satisfaction.
func (p *promotion) loadEmbedClosure(ctx context.Context) (map[domain.NodeID][]domain.NodeID, error) {
	rows, err := p.tx.QueryContext(ctx,
		`SELECT src_node_id, dst_node_id FROM edges WHERE repo_id = ? AND branch = ? AND kind = 'EMBEDS'`,
		p.repoID, p.branch)
	if err != nil {
		return nil, fmt.Errorf("promoter: load embeds: %w", err)
	}
	defer rows.Close()
	out := make(map[domain.NodeID][]domain.NodeID)
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, fmt.Errorf("promoter: scan embed: %w", err)
		}
		out[domain.NodeID(src)] = append(out[domain.NodeID(src)], domain.NodeID(dst))
	}
	return out, rows.Err()
}

// parseMethodSig builds a normalized method signature for set comparison. name is
// the bare method name; signature is the stored "Name(params) results"; snippet
// is the raw declaration, used only to detect a pointer receiver.
func parseMethodSig(name, signature, snippet string) methodSig {
	sig := signature
	if sig == "" {
		sig = snippet // interface methods may carry only the raw element text
	}
	norm, arity, exact, qualified := normalizeSignature(sig)
	return methodSig{
		name:      name,
		norm:      norm,
		arity:     arity,
		exact:     exact,
		qualified: qualified,
		pointer:   receiverIsPointer(snippet),
	}
}

// receiverIsPointer reports whether a concrete method's declaration uses a
// pointer receiver, parsed from the snippet prefix "func (r *T) ...".
func receiverIsPointer(snippet string) bool {
	s := strings.TrimSpace(snippet)
	if !strings.HasPrefix(s, "func") {
		return false
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return false
	}
	rel := strings.IndexByte(s[open:], ')')
	if rel < 0 {
		return false
	}
	return strings.Contains(s[open:open+rel], "*")
}

// normalizeSignature reduces a method signature to a name-independent canonical
// form for comparison. It strips the leading method name and parses the
// remaining "(params) results" as a func type via go/parser (no type-checking,
// no build), rendering parameter and result TYPES without their names. Package
// qualifiers are reduced to the base type name (io.Reader -> Reader) so a type
// in one package matches an interface in another; that reduction is reported via
// `qualified` so callers can downgrade confidence. When parsing fails, norm is
// empty and only `arity` (param/result counts) is trustworthy.
//
// Known gaps: cross-package base-name reduction can over-match two distinct
// types that share a base name; type aliases are not resolved.
func normalizeSignature(sig string) (norm, arity string, exact, qualified bool) {
	open := strings.IndexByte(sig, '(')
	if open < 0 {
		return "", "0|0", false, false
	}
	expr := "func" + sig[open:]
	parsed, err := goparser.ParseExpr(expr)
	if err != nil {
		return "", "0|0", false, false
	}
	ft, ok := parsed.(*goast.FuncType)
	if !ok {
		return "", "0|0", false, false
	}
	var q bool
	renderList := func(fl *goast.FieldList) (string, int) {
		if fl == nil {
			return "", 0
		}
		var parts []string
		for _, f := range fl.List {
			ts, fq := exprTypeString(f.Type)
			if fq {
				q = true
			}
			// One field can declare multiple names of the same type (a, b int).
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			for range n {
				parts = append(parts, ts)
			}
		}
		return strings.Join(parts, ","), len(parts)
	}
	params, np := renderList(ft.Params)
	results, nr := renderList(ft.Results)
	norm = "(" + params + ")->(" + results + ")"
	arity = fmt.Sprintf("%d|%d", np, nr)
	return norm, arity, true, q
}

// exprTypeString renders a type expression to a canonical string, reducing a
// package-qualified type to its base name. The bool reports whether such a
// reduction happened.
func exprTypeString(e goast.Expr) (string, bool) {
	switch t := e.(type) {
	case *goast.Ident:
		return t.Name, false
	case *goast.SelectorExpr:
		return t.Sel.Name, true // pkg.Type -> Type (cross-package normalization)
	case *goast.StarExpr:
		s, q := exprTypeString(t.X)
		return "*" + s, q
	case *goast.ArrayType:
		s, q := exprTypeString(t.Elt)
		if t.Len == nil {
			return "[]" + s, q
		}
		return "[N]" + s, q
	case *goast.Ellipsis:
		s, q := exprTypeString(t.Elt)
		return "..." + s, q
	case *goast.MapType:
		k, q1 := exprTypeString(t.Key)
		v, q2 := exprTypeString(t.Value)
		return "map[" + k + "]" + v, q1 || q2
	case *goast.ChanType:
		s, q := exprTypeString(t.Value)
		return "chan " + s, q
	case *goast.InterfaceType:
		return "interface{}", false
	case *goast.StructType:
		return "struct{}", false
	case *goast.FuncType:
		return "func", false
	default:
		return "?", false
	}
}

// isGenericDecl reports whether a type declaration is generic (has type
// parameters), e.g. "type List[T any] struct{...}". Generics are out of scope
// for satisfaction, so they are skipped rather than mis-resolved.
func isGenericDecl(snippet, name string) bool {
	_, rest, found := strings.Cut(snippet, name)
	if !found {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(rest), "[")
}
