; calls.scm — call-expression patterns (solov2-1yev phase 3a).
;
; Two shapes cover the bulk of legacy collectCallNames:
;   1. identifier(...)               — plain in-package call
;   2. operand.field(...)            — selector_expression where the
;                                      operand is an identifier
;
; Phase 3a runs this query scoped to each function/method body subtree
; (via QueryCursor.Exec(query, bodyNode)) so caller identity is known
; from context and doesn't need to be encoded in the pattern. The
; chained-selector case (operand itself a selector_expression) is
; phase 3b territory and is intentionally NOT captured here — the
; legacy parser drops those into a separate code path with struct
; field type lookup, which deserves its own query / extractor pair.

(call_expression
  function: (identifier) @call.identifier) @call.expr

(call_expression
  function: (selector_expression
    operand: (identifier) @call.operand
    field: (field_identifier) @call.field)) @call.expr

; chained-selector call: `recvName.field.Method()` or `localVar.X.Y()`.
; The Go-side extractor uses the file-wide struct-field-type map +
; per-body local-var-origin map (built once per function) to classify
; each match into either an in-file FieldType.Method edge or an
; UnresolvedCall (with PkgQualifier + IsMethodCall=true). solov2-9rc2.
(call_expression
  function: (selector_expression
    operand: (selector_expression
      operand: (identifier) @call.chain_operand
      field: (field_identifier) @call.chain_field)
    field: (field_identifier) @call.field)) @call.expr
