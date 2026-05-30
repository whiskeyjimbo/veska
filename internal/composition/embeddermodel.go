package composition

import "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"

// The model2vec package deliberately stays model-agnostic — concrete model
// manifests (HuggingFace source + pinned revision + per-file shas) live here,
// alongside the composition root, exactly as model2vec's package doc directs.
// `veska install model2vec` resolves the spec from here so the CLI delivery
// layer carries no model provenance data of its own.

// PotionCode16MName is the model2vec static code embedder's directory name
// (also the model2vec ModelID suffix) and the dir under
// <VeskaHome>/static-model/. It matches model2vec.EmbeddedName so the
// installed-on-disk model and the binary-baked one share an identity.
const PotionCode16MName = "potion-code-16M"

// potionCode16MRev pins the HuggingFace source to a commit revision (not
// `main`) so the download is reproducible; the per-file sha256s below are
// verified after fetch, so a moved/edited upstream file fails loudly rather
// than silently embedding against different weights.
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
