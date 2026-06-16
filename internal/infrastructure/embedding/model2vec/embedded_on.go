//go:build embed_model

// Fat-binary build: the model2vec weights are compiled into
// the binary via //go:embed, so the embedder needs no `veska install` and
// no network. The assets are fetched into./assets/ at build time by
// `make build-fat` and are.gitignore'd — they are NOT committed (a ~62MB
// blob in git history is the cost this design exists to avoid).
package model2vec

import _ "embed"

//go:embed assets/potion-code-16M/tokenizer.json
var embeddedTokenizer []byte

//go:embed assets/potion-code-16M/model.safetensors
var embeddedSafetensors []byte

// EmbeddedName is the model whose weights are compiled into this binary.
// It MUST match the on-disk static-model dir name for the same version so
// the embedded and installed providers share one ModelID (no reindex when
// switching between fat and thin binaries).
const EmbeddedName = "potion-code-16M"

// Embedded returns a Provider backed by the compiled-in weights. The bool
// is true in fat builds. Errors (corrupt embed) collapse to false so the
// caller falls back down the election ladder rather than crashing boot.
func Embedded() (*Provider, bool) {
	p, err := NewFromBytes(EmbeddedName, embeddedTokenizer, embeddedSafetensors)
	if err != nil {
		return nil, false
	}
	return p, true
}
