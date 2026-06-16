//go:build eval

// Markdown table generator for the embed-models bench.
// Reads the runResults captured during a bench run, aggregates per
// model across corpora, and writes a published comparison table to
// The table is the deliverable
// users consult when picking an embedder.
// What aggregates per model:
//   Average recall@10 on "headline" GT across CODE corpora (the
//     published headline metric for code retrieval).
//   Average recall@10 on "headline" GT across PROSE corpora.
//   Average per-query embed latency (model2vec: CPU in-process;
//     ollama: HTTP round-trip — REPORTED SEPARATELY since they aren't
//     comparable).
//   Disk footprint for model2vec (size of the model dir). Ollama
//     models are server-side; cell reads "server-side".
// "Recommended for" is derived from heuristics: any model that wins
// (or ties within 0.02 of the best) on code-recall picks up a "code"
// badge; same for prose; the smallest model2vec by footprint picks up
// "smallest footprint"; the lowest-latency model picks up "fastest
// query". Models can earn multiple badges.

package embed_models

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// codeCorpora is the set of corpus names whose embed-time results
// contribute to the published "code recall" cell. Mirrors the kind
// classification in discoverCorpora — but ground-truth headlines come
// from fixtures/headline.jsonl only for veska today; the bench
// curator extends as needed and the aggregate adapts automatically.
var codeCorporaSet = map[string]bool{
	"veska": true, "cobra": true, "pflag": true, "testify": true, "gin": true,
}

// proseCorporaSet — mirror for prose.
var proseCorporaSet = map[string]bool{
	"veska-docs": true, "cobra-docs": true, "wikipedia-tech": true,
}

// writeMarkdownTable aggregates the in-memory results and produces the
// published comparison table. Called at the end of the bench so a
// single 'make eval-embed-models[-full]' invocation refreshes the
// published doc. Path is overridable via EMBED_BENCH_TABLE_OUT;
// default is
func writeMarkdownTable(rows []runResult) error {
	if len(rows) == 0 {
		return nil
	}

	type agg struct {
		model     string
		modelType string
		codeR10   float64 // mean recall@10 on headline GT across code corpora
		proseR10  float64 // mean recall@10 on headline GT across prose corpora
		queryMS   float64 // mean per-query embed latency across all corpora
		diskBytes int64   // model dir size (model2vec); -1 for ollama
		nCode     int     // count of code corpora that contributed a headline number
		nProse    int     // count of prose corpora that contributed
		sumR10C   float64
		sumR10P   float64
		sumQ      float64
		nQ        int
		// Condensation axis (oo4q.2). Populated only when ANY row in
		// rows has CondensedRecall set, which happens under
		// EMBED_BENCH_CONDENSE=on. The Lift columns and condensation
		// section in the markdown are emitted only when hasCond.
		hasCond      bool
		codeR10Cond  float64
		proseR10Cond float64
		nCodeCond    int
		nProseCond   int
		sumR10Cc     float64
		sumR10Pc     float64
	}

	byModel := map[string]*agg{}
	for _, r := range rows {
		a, ok := byModel[r.Model]
		if !ok {
			a = &agg{model: r.Model, modelType: r.ModelType, diskBytes: -1}
			byModel[r.Model] = a
		}
		a.sumQ += r.QueryMS
		a.nQ++
		hl, ok := r.Recall["headline"]
		if !ok || hl.N == 0 {
			continue
		}
		// Use the fair series for cross-model comparison: it divides by
		// (N - NotInCorpus) so models running on a smaller embed-time
		// subset (Ollama at max_docs=500 vs model2vec at 5000) aren't
		// penalised for targets that simply weren't embedded. Falls
		// back to raw when FairN == 0 (every target was missing).
		fair := hl.FairAt10
		if hl.FairN == 0 {
			fair = hl.At10
		}
		if codeCorporaSet[r.Corpus] {
			a.sumR10C += fair
			a.nCode++
		} else if proseCorporaSet[r.Corpus] {
			a.sumR10P += fair
			a.nProse++
		}
		// Condensed series (only when the run actually produced one).
		if r.CondensedRecall != nil {
			a.hasCond = true
			if chl, ok := r.CondensedRecall["headline"]; ok && chl.N > 0 {
				cfair := chl.FairAt10
				if chl.FairN == 0 {
					cfair = chl.At10
				}
				if codeCorporaSet[r.Corpus] {
					a.sumR10Cc += cfair
					a.nCodeCond++
				} else if proseCorporaSet[r.Corpus] {
					a.sumR10Pc += cfair
					a.nProseCond++
				}
			}
		}
	}

	// Average + resolve disk footprint.
	for _, a := range byModel {
		if a.nCode > 0 {
			a.codeR10 = a.sumR10C / float64(a.nCode)
		}
		if a.nProse > 0 {
			a.proseR10 = a.sumR10P / float64(a.nProse)
		}
		if a.nCodeCond > 0 {
			a.codeR10Cond = a.sumR10Cc / float64(a.nCodeCond)
		}
		if a.nProseCond > 0 {
			a.proseR10Cond = a.sumR10Pc / float64(a.nProseCond)
		}
		if a.nQ > 0 {
			a.queryMS = a.sumQ / float64(a.nQ)
		}
		if a.modelType == "model2vec" {
			a.diskBytes = modelDirBytes(a.model)
		}
	}

	// Sort: model2vec first (footprint-ascending), then ollama
	// (alphabetical). Makes the table easier to read.
	models := make([]*agg, 0, len(byModel))
	for _, a := range byModel {
		models = append(models, a)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].modelType != models[j].modelType {
			return models[i].modelType == "model2vec"
		}
		if models[i].modelType == "model2vec" {
			if models[i].diskBytes != models[j].diskBytes {
				return models[i].diskBytes < models[j].diskBytes
			}
		}
		return models[i].model < models[j].model
	})

	// Compute axis winners for the Recommended-for column. A model
	// ties for "best" if it's within 0.02 of the leader on that axis.
	const tieEps = 0.02
	bestCode, bestProse := 0.0, 0.0
	var smallestDisk int64 = -1
	bestQuery := -1.0
	for _, a := range models {
		if a.codeR10 > bestCode {
			bestCode = a.codeR10
		}
		if a.proseR10 > bestProse {
			bestProse = a.proseR10
		}
		if a.diskBytes > 0 && (smallestDisk == -1 || a.diskBytes < smallestDisk) {
			smallestDisk = a.diskBytes
		}
		if a.queryMS > 0 && (bestQuery == -1 || a.queryMS < bestQuery) {
			bestQuery = a.queryMS
		}
	}

	// Build rows.
	var sb strings.Builder
	sb.WriteString("# Embedder Benchmarks\n\n")
	sb.WriteString("Auto-generated by `make eval-embed-models` / `make eval-embed-models-full`.\n")
	sb.WriteString("Source: `tools/loadtest/embed_models/out/results.json`. Bead: `solov2-0k5h`.\n\n")
	sb.WriteString("Recall numbers are **fair** recall@10 on the hand-curated headline set ")
	sb.WriteString("(`fixtures/headline.jsonl` + `fixtures/prose.jsonl`), averaged over corpora that contributed a headline number. ")
	sb.WriteString("'Fair' = recall computed over the pairs whose expected target was actually in the embedded subset — corrects for the different `max_docs` cap between model2vec (5000) and Ollama (500) so models running on different subset sizes aren't penalised for missing targets that weren't embedded at all. ")
	sb.WriteString("Raw R@10 and the per-corpus breakdown live in `results.json`.\n")
	sb.WriteString("Latency is per-query embed time. ")
	sb.WriteString("**model2vec rows measure in-process CPU compute; ollama rows measure HTTP round-trip — do NOT compare across types.**\n\n")
	sb.WriteString("| Model | Type | Size | Code R@10 | Prose R@10 | Query ms | Recommended for |\n")
	sb.WriteString("|---|---|---|---|---|---|---|\n")
	for _, a := range models {
		size := "n/a"
		switch {
		case a.modelType == "ollama":
			size = "server-side"
		case a.diskBytes > 0:
			size = humanBytes(a.diskBytes)
		}
		codeStr := fmtRecall(a.codeR10, a.nCode)
		proseStr := fmtRecall(a.proseR10, a.nProse)
		var badges []string
		if a.codeR10 > 0 && bestCode-a.codeR10 <= tieEps {
			badges = append(badges, "code")
		}
		if a.proseR10 > 0 && bestProse-a.proseR10 <= tieEps {
			badges = append(badges, "prose")
		}
		if a.modelType == "model2vec" && smallestDisk > 0 && a.diskBytes == smallestDisk {
			badges = append(badges, "smallest footprint")
		}
		if a.queryMS > 0 && bestQuery > 0 && a.queryMS <= bestQuery*1.10 {
			// 10% slack — many fast models bunch up
			badges = append(badges, "fastest query")
		}
		recommend := strings.Join(badges, ", ")
		if recommend == "" {
			recommend = "—"
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s | %s | %.3f | %s |\n",
			a.model, a.modelType, size, codeStr, proseStr, a.queryMS, recommend)
	}

	// Condensation lift table (oo4q.2): only when at least one model
	// has a condensed series. Reports the per-model lift (condensed
	// raw) on code and prose headline R@10, plus the share of corpus
	// docs that actually got condensed (the rest were below the
	// minLen gate and embedded raw — so they cap the achievable lift).
	anyCond := false
	for _, a := range models {
		if a.hasCond {
			anyCond = true
			break
		}
	}
	if anyCond {
		sb.WriteString("\n## Condensation lift (EMBED_BENCH_CONDENSE=on)\n\n")
		sb.WriteString("Extractive condensation: per doc, split into pieces (lines), embed each, ")
		sb.WriteString("keep top-K most-central by cosine-centroid, concatenate, re-embed. ")
		sb.WriteString("Docs below `EMBED_BENCH_CONDENSE_MIN_LEN` are embedded raw — the gated subset caps the achievable lift. ")
		sb.WriteString("Positive lift = condensed beats raw.\n\n")
		sb.WriteString("| Model | Code R@10 (cond) | Code Lift | Prose R@10 (cond) | Prose Lift |\n")
		sb.WriteString("|---|---|---|---|---|\n")
		for _, a := range models {
			if !a.hasCond {
				continue
			}
			codeCondStr := fmtRecall(a.codeR10Cond, a.nCodeCond)
			proseCondStr := fmtRecall(a.proseR10Cond, a.nProseCond)
			codeLift := "—"
			if a.nCode > 0 && a.nCodeCond > 0 {
				codeLift = fmt.Sprintf("%+.3f", a.codeR10Cond-a.codeR10)
			}
			proseLift := "—"
			if a.nProse > 0 && a.nProseCond > 0 {
				proseLift = fmt.Sprintf("%+.3f", a.proseR10Cond-a.proseR10)
			}
			fmt.Fprintf(&sb, "| `%s` | %s | %s | %s | %s |\n",
				a.model, codeCondStr, codeLift, proseCondStr, proseLift)
		}
	}

	sb.WriteString("\n## Notes\n\n")
	sb.WriteString("- An empty Code or Prose cell means no headline GT pairs ran for that model on the corresponding corpora.\n")
	sb.WriteString("- A model's full per-corpus, per-GT-source recall (including the auto-generated doc-derived and test-name-derived sets) lives in `tools/loadtest/embed_models/out/results.json`.\n")
	sb.WriteString("- Reproduce by installing the model2vec set (`tools/loadtest/embed_models/scripts/install-bench-models.sh`), fetching corpora (`tools/loadtest/embed_models/scripts/fetch-corpora.sh`), then running `make eval-embed-models` (or `-full` to add the Ollama set).\n")

	out := os.Getenv("EMBED_BENCH_TABLE_OUT")
	if out == "" {
		_, file, _, _ := runtime.Caller(0)
		repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
		out = filepath.Join(repoRoot, "docs", "operations", "embedder-benchmarks.md")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, []byte(sb.String()), 0o644)
}

func fmtRecall(v float64, n int) string {
	if n == 0 {
		return "—"
	}
	return fmt.Sprintf("%.3f", v)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dMB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dKB", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}

// modelDirBytes sums the sizes of every file under
// $VESKA_HOME/static-model/<model>/ — what `veska install <model>`
// costs on disk. Returns -1 if the dir doesn't exist.
func modelDirBytes(modelName string) int64 {
	dir := filepath.Join(modelRoot(), modelName)
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil || total == 0 {
		return -1
	}
	return total
}
