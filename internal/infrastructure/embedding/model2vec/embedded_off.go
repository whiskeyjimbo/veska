//go:build !embed_model

package model2vec

// Embedded returns a negative indicator since no model is compiled into this thin build.
func Embedded() (*Provider, bool) { return nil, false }
