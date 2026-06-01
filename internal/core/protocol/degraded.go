// Package protocol holds the wire-protocol vocabulary shared between the MCP
// server (infrastructure) and the CLI (delivery). It is a pure inner-layer
// leaf with no internal imports, so both an outer adapter and a delivery
// command can depend on these contract constants inward from core rather than
// one outer package importing another (which would invert the dependency
// direction; solov2-geam).
package protocol

// DegradedReasonChainedSelectorsUnresolved is emitted on eng_get_call_chain
// responses when the seed node is a callable whose body contains chained
// selector call sites (e.g. cobra's `rootCmd.PersistentFlags().StringVarP(...)`,
// or `s.field.M()`) that the tree-sitter extractor does not yet model as
// edges — see epic solov2-9rc2. Agents should treat an empty edges array
// on a callable carrying this reason as "parser limitation, may not be
// authoritative."
const DegradedReasonChainedSelectorsUnresolved = "chained_selectors_unresolved"

// DegradedReasonExternalCalleesOnly is emitted when the seed callable's
// body has no chained selectors but also produced no resolvable CALLS
// edges. The dominant cause is that every callee lives outside the
// indexed graph (stdlib like fmt/strings, or third-party packages from
// unregistered modules). An agent reading this should NOT conclude the
// parser is buggy — the empty edges set reflects the index boundary,
// not a parser limitation (solov2-izh6.22).
const DegradedReasonExternalCalleesOnly = "external_callees_only"

// DegradedReasonIndexingInProgress is emitted on any read tool that
// returned an empty result while at least one cold scan was still
// running. A query that hits the daemon during the cold-scan window
// would otherwise see {nodes:[]} silently and conclude the symbol does
// not exist; this reason tells the caller to retry once indexing
// settles (solov2-izh6.30). The accompanying IndexingRepos field, when
// populated, lists the repo_ids the caller should wait on.
const DegradedReasonIndexingInProgress = "indexing_in_progress"
