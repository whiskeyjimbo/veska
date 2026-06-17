// Package protocol holds the wire-protocol vocabulary shared between the MCP
// server (infrastructure) and the CLI (delivery). It is a pure inner-layer
// leaf with no internal imports, so both an outer adapter and a delivery
// command can depend on these contract constants inward from core rather than
// one outer package importing another (which would invert the dependency
// direction).
package protocol

// DegradedReasonChainedSelectorsUnresolved is emitted on eng_get_call_chain
// responses when the seed node contains chained selector call sites that the
// extractor does not model as edges. Agents should treat an empty edges array
// carrying this reason as a parser limitation that may not be authoritative.
const DegradedReasonChainedSelectorsUnresolved = "chained_selectors_unresolved"

// DegradedReasonExternalCalleesOnly is emitted when the seed callable's body
// has no chained selectors but produced no resolvable CALLS edges. The main
// cause is that every callee lives outside the indexed graph. An agent reading
// this should not conclude the parser is buggy; the empty edges set reflects
// the index boundary, not a parser limitation.
const DegradedReasonExternalCalleesOnly = "external_callees_only"

// DegradedReasonIndexingInProgress is emitted on any read tool that returned
// an empty result while at least one cold scan was running. This tells the
// caller to retry once indexing settles. The accompanying IndexingRepos field
// lists the repository IDs the caller should wait on.
const DegradedReasonIndexingInProgress = "indexing_in_progress"

// DegradedReasonWakeReconciling is emitted on a graph read tool whenever a
// repository touched by the query has an in-flight wake reconcile sweep. Unlike
// indexing_in_progress, this fires on empty and non-empty results because a sweep
// may be mid-flight re-parsing files, making even a populated response
// momentarily stale. The accompanying WakeReconcilingRepos field lists the
// affected repository IDs.
const DegradedReasonWakeReconciling = "wake_reconciling"
