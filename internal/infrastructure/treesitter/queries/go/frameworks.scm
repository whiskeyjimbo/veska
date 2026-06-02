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
; First framework: spf13/cobra. urfave/cli, kong, and HTTP routers
; (gin/echo → KindRoute) are reserved follow-ups; each drops in here as
; another pattern + a branch in go_frameworks.go without touching the
; generic symbol/call extractors.
;
; The package qualifier (@cobra.cmd.pkg) is verified against the file
; import map Go-side — matching the type name "Command" alone would
; misfire on any unrelated `foo.Command{}` literal.

; ungrouped: var X = &cobra.Command{ Use: "...", ... }
(source_file
  (var_declaration
    (var_spec
      name: (identifier) @cobra.cmd.var
      value: (expression_list
        (unary_expression
          operand: (composite_literal
            type: (qualified_type
              package: (package_identifier) @cobra.cmd.pkg
              name: (type_identifier) @cobra.cmd.type)
            body: (literal_value) @cobra.cmd.body))))) @cobra.cmd.decl)

; grouped: var ( X = &cobra.Command{ ... } )
(source_file
  (var_declaration
    (var_spec_list
      (var_spec
        name: (identifier) @cobra.cmd.var
        value: (expression_list
          (unary_expression
            operand: (composite_literal
              type: (qualified_type
                package: (package_identifier) @cobra.cmd.pkg
                name: (type_identifier) @cobra.cmd.type)
              body: (literal_value) @cobra.cmd.body)))))) @cobra.cmd.decl)

; wire-up: parent.AddCommand(child, ...). Matches any selector call;
; go_frameworks.go filters on field == "AddCommand" and maps each
; identifier argument back to a command node via the var-name binding
; built from the patterns above.
(call_expression
  function: (selector_expression
    operand: (identifier) @cobra.add.parent
    field: (field_identifier) @cobra.add.method)
  arguments: (argument_list) @cobra.add.args)
