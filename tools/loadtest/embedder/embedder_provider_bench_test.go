// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

// Embedder provider micro-benchmarks: compare per-embed
// throughput and one-time load cost across the election ladder
// static-v2 (in-binary), model2vec from disk, and model2vec embedded
// (fat build). These informed the fat/thin packaging decision:
//
//	model2vec Embed is ~500x faster than static-v2 (table lookup +
//	  weighted mean vs n-gram hashing), and higher quality - so static-v2
//	  is a weak last resort, not a "fast lightweight" option.
//	Using go:embed adds no load-time penalty vs reading the file from disk.
//	The embedded path keeps the ~62MB weights resident for the process
//	  lifetime (binary data section), which is the fat binary's RSS cost
//	  over thin+install. Run a process-RSS check separately for that.
//
// Run: `make eval-embedder-bench`. The embedded arm requires the fat
// build tag (`-tags 'eval embed_model'`) and skips otherwise; the disk
// arms skip when model2vec is not installed under VESKA_HOME.
package embedder

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	embedstatic "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// benchEmbedText is a representative code snippet - the kind of projection
// text the embedder worker sees in production.
const benchEmbedText = `func (p *Promoter) Promote(ctx context.Context, repoID, branch, sha string) (Result, error) {
	// atomic promotion: all SQL behind PromotionStore; co-transactional sinks run inline
	return p.store.WithTx(ctx, func(tx PromotionTx) error { return p.apply(tx, repoID, branch, sha) })
}`

// model2vecDir resolves the installed model dir, or "" when absent.
func model2vecDir() string {
	dir := model2vec.ModelDir(config.DefaultVectorDir(), "potion-code-16M")
	if _, err := os.Stat(filepath.Join(dir, "model.safetensors")); err != nil {
		return ""
	}
	return dir
}

func BenchmarkLoad_Static(b *testing.B) {
	for b.Loop() {
		if _, err := embedstatic.New(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoad_Model2VecDisk(b *testing.B) {
	dir := model2vecDir()
	if dir == "" {
		b.Skip("model2vec not installed under VESKA_HOME - run 'veska install model2vec'")
	}
	for b.Loop() {
		if _, err := model2vec.New(dir); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoad_Model2VecEmbedded(b *testing.B) {
	if _, ok := model2vec.Embedded(); !ok {
		b.Skip("not a fat build - rebuild with -tags 'eval embed_model'")
	}
	for b.Loop() {
		if _, ok := model2vec.Embedded(); !ok {
			b.Fatal("embedded model vanished")
		}
	}
}

func BenchmarkEmbed_Static(b *testing.B) {
	p, err := embedstatic.New()
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := p.Embed(ctx, benchEmbedText); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEmbed_Model2Vec(b *testing.B) {
	dir := model2vecDir()
	if dir == "" {
		b.Skip("model2vec not installed under VESKA_HOME - run 'veska install model2vec'")
	}
	p, err := model2vec.New(dir)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := p.Embed(ctx, benchEmbedText); err != nil {
			b.Fatal(err)
		}
	}
}
