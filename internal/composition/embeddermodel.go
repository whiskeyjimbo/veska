package composition

import "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"

// The model2vec package is model-agnostic. Concrete model manifests (HuggingFace
// source, pinned revision, and per-file SHAs) live here alongside the composition
// root so the delivery layer carries no model provenance data of its own.

// PotionCode16MName is the model2vec static code embedder's directory name. It
// matches model2vec.EmbeddedName so the installed-on-disk model and the
// binary-baked one share an identity.
const PotionCode16MName = "potion-code-16M"

// potionCode16MRev pins the HuggingFace source to a specific commit revision so
// the download is reproducible. Verifying per-file SHA256 hashes ensures a
// moved or edited upstream file fails loudly rather than silently embedding
// against different weights.
const potionCode16MRev = "86848193a842865570d9c8d3e7d268b66ab52752"

// PotionCode16MSpec returns the download manifest for the potion-code-16M
// static code embedder fetched by `veska install model2vec`.
func PotionCode16MSpec() model2vec.ModelSpec {
	return model2vec.ModelSpec{
		BaseURL: "https://huggingface.co/minishlab/" + PotionCode16MName + "/resolve/" + potionCode16MRev,
		Files: []model2vec.FileSpec{
			{Name: "tokenizer.json", SHA256: "8e84217af15e70e8127c855435fc3d8a4cd91d7bbe686f72e75f188118ec78ae"},
			{Name: "model.safetensors", SHA256: "ca6159081a6e96cebe4ad878e5e8437bfccc761e8db16223370149cd2faa6c0b"},
		},
	}
}
