; imports.scm - import declarations.
;
; Both bare and aliased import_specs surface; the Go-side extractor
; walks each spec for an optional name child (alias). Path values
; arrive as interpreted_string_literal (the quoted form) - the Go-side
; strips the quotes. Blank ("_") and dot (".") imports are filtered
; out at the extractor because their effect is package init, not a
; usable qualifier (matches the legacy extractImports skip).

(import_spec
  path: (interpreted_string_literal) @import.path) @import.spec
