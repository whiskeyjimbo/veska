; symbols.scm — declarative declaration extraction for Go (solov2-1yev).
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
; tree-sitter-go tags.scm upstream — both Apache-2.0/MIT — with our
; capture names. Vendoring the wider set comes in later phases.

(function_declaration
  name: (identifier) @function.name) @function.decl
