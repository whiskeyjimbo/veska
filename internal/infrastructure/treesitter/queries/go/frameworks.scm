; frameworks.scm — framework-aware symbol extraction (solov2-crn7).
;
; Where a generic top-level var (symbols.scm) only sees `var rootCmd =
; &cobra.Command{...}` as an opaque KindVariable named "rootCmd", these
; patterns recognise the framework's command struct-literal and the
; wire-up calls that build the command tree. The Go extractor
; (go_frameworks.go) promotes matches to KindCommand nodes named by the
; framework's command word (cobra Use:) and emits CONTAINS edges from
; AddCommand(...) so call_chain / blast_radius walk the actual tree.
;
; Frameworks handled: spf13/cobra (Command → command named by Use:),
; urfave/cli (App → command named by Name:, with its Commands:[]*Command
; slice as subcommands), and HTTP routers gin/echo/chi (router.METHOD(
; "/path", handler) → KindRoute named "METHOD /path" + a ROUTES
; route→handler edge resolved at promotion — solov2-ketg). kong (struct
; tags) is a reserved follow-up; it drops in as a branch in
; go_frameworks.go.
;
; The @fwvar.* patterns capture EVERY top-level `var X = &pkg.Type{...}`;
; go_frameworks.go dispatches on (resolved import path, type name) so a
; single pattern serves all composite-literal frameworks. Matching the
; type name alone would misfire on any unrelated `foo.Command{}`, so the
; package qualifier is always verified against the file import map.

; ungrouped: var X = &pkg.Type{ ... }
(source_file
  (var_declaration
    (var_spec
      name: (identifier) @fwvar.name
      value: (expression_list
        (unary_expression
          operand: (composite_literal
            type: (qualified_type
              package: (package_identifier) @fwvar.pkg
              name: (type_identifier) @fwvar.type)
            body: (literal_value) @fwvar.body))))) @fwvar.decl)

; grouped: var ( X = &pkg.Type{ ... } )
(source_file
  (var_declaration
    (var_spec_list
      (var_spec
        name: (identifier) @fwvar.name
        value: (expression_list
          (unary_expression
            operand: (composite_literal
              type: (qualified_type
                package: (package_identifier) @fwvar.pkg
                name: (type_identifier) @fwvar.type)
              body: (literal_value) @fwvar.body)))))) @fwvar.decl)

; wire-up: parent.AddCommand(child, ...). Matches any selector call;
; go_frameworks.go filters on field == "AddCommand" and maps each
; identifier argument back to a command node via the var-name binding
; built from the patterns above.
(call_expression
  function: (selector_expression
    operand: (identifier) @cobra.add.parent
    field: (field_identifier) @cobra.add.method)
  arguments: (argument_list) @cobra.add.args)

; HTTP route: router.METHOD("/path", handler). Matches any selector call;
; go_frameworks.go filters field against the HTTP verb set (gin/echo GET,
; chi Get) and verifies the first arg is a string literal with a handler
; arg present (the four-way precision gate). The operand is intentionally
; uncaptured — the router is a param of an unresolved type, so it can't be
; verified by receiver type; precision comes from the import + verb + arg
; gate instead (solov2-ketg).
(call_expression
  function: (selector_expression
    field: (field_identifier) @route.method)
  arguments: (argument_list) @route.args) @route.call
