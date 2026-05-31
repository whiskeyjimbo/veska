// Package composition holds composition-root helpers for the daemon
// (internal/cli/daemon) and the CLI (cmd/veska) entry points. Most are shared
// by both — the cold-scan ingestion/promotion core and the wiki handler — so
// the wiring is defined once instead of duplicated and kept in lock-step by
// hand-written comments (solov2-u4mv); a few (the CLI search service) are
// entry-point-specific construction relocated here to keep the Cobra commands
// thin adapters.
//
// It is a composition root, so it may import the infrastructure adapters; the
// hexagonal rule that domain/ports must not import infra still holds and is
// enforced by `make layercheck`.
package composition
