// Package composition holds composition-root helpers for the daemon and CLI
// entry points. It defines shared wiring for the cold-scan ingestion/promotion
// core and the wiki handler, keeping entry points as thin adapters. As the
// composition root, it may import infrastructure adapters, while keeping
// domain and ports packages clean of infrastructure imports.
package composition
