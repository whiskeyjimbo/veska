// Package protocol holds the wire-protocol vocabulary shared between the MCP
// server (infrastructure) and the CLI (delivery). It is a pure inner-layer
// leaf with no internal imports, so both an outer adapter and a delivery
// command can depend on these contract constants inward from core rather than
// one outer package importing another (which would invert the dependency
// direction).
package protocol

// DegradedReasonChainedSelectorsUnresolved is emitted on eng_get_call_chain
// responses when the seed node is a callable whose body contains chained
// selector call sites (e.g. cobra's `rootCmd.PersistentFlags.StringVarP(.)`,
// or `s.field.M`) that the tree-sitter extractor does not yet model as
// edges — see epic. Agents should treat an empty edges array
// on a callable carrying this reason as "parser limitation, may not be
// authoritative."
const DegradedReasonChainedSelectorsUnresolved = "chained_selectors_unresolved"

// DegradedReasonExternalCalleesOnly is emitted when the seed callable's
// body has no chained selectors but also produced no resolvable CALLS
// edges. The dominant cause is that every callee lives outside the
// indexed graph (stdlib like fmt/strings, or third-party packages from
// unregistered modules). An agent reading this should NOT conclude the
// parser is buggy — the empty edges set reflects the index boundary,
// not a parser limitation.
const DegradedReasonExternalCalleesOnly = "external_callees_only"

// DegradedReasonIndexingInProgress is emitted on any read tool that
// returned an empty result while at least one cold scan was still
// running. A query that hits the daemon during the cold-scan window
// would otherwise see {nodes:} silently and conclude the symbol does
// not exist; this reason tells the caller to retry once indexing
// settles. The accompanying IndexingRepos field, when
// populated, lists the repo_ids the caller should wait on.
const DegradedReasonIndexingInProgress = "indexing_in_progress"

// DegradedReasonWakeReconciling is emitted on a graph read tool whenever a
// repo touched by the query has an in-flight wake reconcile sweep (a
// suspend/resume mtime re-scan). Unlike indexing_in_progress this fires on
// empty AND non-empty results: a sweep may be mid-flight re-parsing files the
// query just read, so even a populated response could be momentarily stale.
// The accompanying WakeReconcilingRepos field lists the affected repo_ids.
const DegradedReasonWakeReconciling = "wake_reconciling"
