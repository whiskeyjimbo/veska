; symbols.scm - declarative declaration extraction for Go .
;
; Capture conventions match what go_query_extract.go expects:
;   @function.decl   the function_declaration node itself
;   @function.name   the identifier child (this becomes the node name)
;
; This is intentionally narrow for phase 1: only top-level function
; declarations. Methods, types, vars/consts land in later phases so the
; query path can be diffed extractor-by-extractor against the legacy
; walkers.
;
; Patterns borrow shape from Helix's runtime/queries/go and the
; tree-sitter-go tags.scm upstream - both Apache-2.0/MIT - with our
; capture names. Vendoring the wider set comes in later phases.

; body captures (@*.body) let the phase 3 call extractor scope its
; calls.scm query to JUST this declaration's body, so caller identity
; is implicit (the function/method we're processing) instead of needing
; a parent walk per call site. body is optional in some interface
; embeddings and forward declarations, so the extractor checks for
; presence before scoping.
(function_declaration
  name: (identifier) @function.name
  body: (block)? @function.body) @function.decl

; method_declaration captures the receiver's parameter_list so the Go
; extractor can run the existing extractReceiverBinding / extractReceiverType
; helpers - those handle pointer vs value receivers and the receiver-name
; binding consistently with parseMethodDecl in the legacy walker.
(method_declaration
  receiver: (parameter_list) @method.receiver
  name: (field_identifier) @method.name
  body: (block)? @method.body) @method.decl

; type_declaration covers struct, interface, and plain alias types. The
; extractor inspects @type.body.Type() to dispatch between
; KindStruct / KindInterface / KindType, matching parseTypeDecl's switch
; in the legacy walker. Capturing the body lets the same extractor also
; collect interface method nodes (solov2-9rc2 phase E v2) without a
; second pass.
;
; Anchored to source_file to match TOP-LEVEL types only - function-local
; type declarations (Go allows `func f(){ type k int; ... }`) are not
; part of the symbol graph, and on real codebases two different funcs
; routinely declare the same local name (hugo: helpers_test.go has
; `type k string` inside two different test functions). Without the
; anchor those collide on (repoID, path, kind, name) → identical
; node_id and the promotion tx fails with UNIQUE-PK on nodes (1555).
; Pinned by solov2-14lw.
(source_file
  (type_declaration
    (type_spec
      name: (type_identifier) @type.name
      type: _ @type.body)) @type.decl)

; var_declaration / const_declaration: anchored to source_file so we
; only match TOP-LEVEL declarations (the legacy parseTopLevelVarDecl
; walks root.Child(i) directly and never descends into function
; bodies). The spec is what we walk in Go because a single spec can
; declare multiple identifiers (`var a, b int`) and tree-sitter
; exposes them as repeated named children rather than field captures.
;
; Two patterns per declaration kind because tree-sitter's Go grammar
; nests the specs differently depending on whether the source uses a
; parenthesised block - `var x = 1` produces var_declaration → var_spec
; directly, while `var ( ... )` produces var_declaration → var_spec_list
; → var_spec. The Go extractor needs both the spec (for identifiers)
; AND the enclosing declaration (its line range + raw content drive
; lineRange / RawContent - legacy parseTopLevelVarSpec uses decl, not
; spec, so a grouped var preserves its full block in raw_content).
(source_file
  (var_declaration
    (var_spec) @var.spec) @var.decl)

(source_file
  (var_declaration
    (var_spec_list
      (var_spec) @var.spec)) @var.decl)

; const_declaration has no spec_list wrapper - const_specs are direct
; children of const_declaration in both the parenthesised and
; non-parenthesised forms, so one pattern covers both.
(source_file
  (const_declaration
    (const_spec) @const.spec) @const.decl)
