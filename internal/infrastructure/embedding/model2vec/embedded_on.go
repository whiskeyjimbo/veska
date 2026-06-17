// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build embed_model

// Package model2vec provides embedded Model2Vec weights and configurations compiled via go:embed.
package model2vec

import _ "embed"

//go:embed assets/potion-code-16M/tokenizer.json
var embeddedTokenizer []byte

//go:embed assets/potion-code-16M/model.safetensors
var embeddedSafetensors []byte

// EmbeddedName is the identifier of the model whose weights are compiled into this binary.
const EmbeddedName = "potion-code-16M"

// Embedded returns the compiled-in Model2Vec provider.
func Embedded() (*Provider, bool) {
	p, err := NewFromBytes(EmbeddedName, embeddedTokenizer, embeddedSafetensors)
	if err != nil {
		return nil, false
	}
	return p, true
}
