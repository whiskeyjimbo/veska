//go:build !embed_model

package model2vec

// Embedded reports no compiled-in model in thin (default) builds. The fat
// build (build tag `embed_model`, see embedded_on.go) replaces this with a
// real provider. Keeping the symbol in both builds lets callers (elect)
// reference Embedded unconditionally.
func Embedded() (*Provider, bool) { return nil, false }
