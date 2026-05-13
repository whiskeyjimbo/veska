//go:build hnsw_native

// cmd/hnsw-eval runs the full HNSW candidate evaluation sweep at 50k and 250k vectors.
//
// It measures:
//   - recall@10 with 100 hold-out queries at each population size
//   - warm p95 query latency at k=10 (200 warm queries)
//   - backup round-trip correctness (5 hold-out queries)
//   - index file size at each quantization level (usearch: float32/float16/int8; others: float32)
//
// Results are printed as a Markdown table and written to RESULTS.md.
//
// Build-time CGo notes:
//   - usearch: requires libusearch_c.so (from usearch v2.25.2 deb) and usearch.h.
//     Set CGO_LDFLAGS="-L<path-to-lib> -lusearch_c" and CGO_CFLAGS="-I<path-to-include>".
//   - lancedb: requires liblancedb_go.a from lancedb-go v0.1.2 release.
//     Set CGO_LDFLAGS="<path>/liblancedb_go.a -lm -ldl -lpthread".
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	usearchlib "github.com/unum-cloud/usearch/golang"

	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/hnsw/cohnsw"
	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/hnsw/eval"
	ldb "github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/hnsw/lancedb"
	uidx "github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/hnsw/usearch"
	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/sqlitevec/gen"
)

const (
	nHoldOut      = 100
	nWarmQueries  = 200
	nRoundTrip    = 5
	nSmall        = 50_000
	nLarge        = 250_000
)

type row struct {
	library    string
	quant      string
	pop        int
	recall10   float64
	p95ms      float64
	fileSizeKB int64
	roundTrip  string
	cgoNote    string
}

func main() {
	tmpDir, err := os.MkdirTemp("", "hnsw-eval-*")
	if err != nil {
		fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	var rows []row
	buildStart := time.Now()

	// --- usearch ---
	for _, pop := range []int{nSmall, nLarge} {
		for _, quant := range []struct {
			name  usearchlib.Quantization
			label string
		}{
			{usearchlib.F32, "float32"},
			{usearchlib.F16, "float16"},
			{usearchlib.I8, "int8"},
		} {
			fmt.Fprintf(os.Stderr, "usearch/%s @%dk: building...\n", quant.label, pop/1000)
			r := evalUsearch(tmpDir, pop, quant.name, quant.label)
			rows = append(rows, r)
		}
	}

	// --- coder/hnsw ---
	for _, pop := range []int{nSmall, nLarge} {
		fmt.Fprintf(os.Stderr, "coder/hnsw @%dk: building...\n", pop/1000)
		r := evalCohnsw(tmpDir, pop)
		rows = append(rows, r)
	}

	// --- lancedb ---
	for _, pop := range []int{nSmall, nLarge} {
		fmt.Fprintf(os.Stderr, "lancedb @%dk: building...\n", pop/1000)
		r := evalLanceDB(tmpDir, pop)
		rows = append(rows, r)
	}

	_ = buildStart // total build time not separately tracked per-library

	// Sort rows for nice output.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].library != rows[j].library {
			return rows[i].library < rows[j].library
		}
		if rows[i].quant != rows[j].quant {
			return rows[i].quant < rows[j].quant
		}
		return rows[i].pop < rows[j].pop
	})

	md := renderMarkdown(rows)
	fmt.Println(md)

	resultsPath := filepath.Join(filepath.Dir(os.Args[0]), "..", "..", "..", "..", "RESULTS.md")
	// Write relative to the spike directory.
	spikePath := findSpikeDir()
	if spikePath != "" {
		resultsPath = filepath.Join(spikePath, "RESULTS.md")
	}
	if err := os.WriteFile(resultsPath, []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write RESULTS.md: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "wrote %s\n", resultsPath)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}

func findSpikeDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// file = .../tools/loadtest/spikes/hnsw/cmd/hnsw-eval/main.go
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// evalUsearch evaluates usearch at the given population and quantization.
func evalUsearch(tmpDir string, pop int, quant usearchlib.Quantization, quantLabel string) row {
	corpus := gen.GenerateVectors(pop, 42)
	holdOut := gen.GenerateVectors(nHoldOut, 8888)

	idx, err := uidx.New(quant)
	if err != nil {
		return row{library: "usearch", quant: quantLabel, pop: pop,
			roundTrip: fmt.Sprintf("ERROR: %v", err), cgoNote: "yes (C++17)"}
	}
	defer idx.Destroy()

	if err := idx.Reserve(uint(pop)); err != nil {
		return row{library: "usearch", quant: quantLabel, pop: pop,
			roundTrip: fmt.Sprintf("RESERVE ERROR: %v", err), cgoNote: "yes (C++17)"}
	}

	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			return row{library: "usearch", quant: quantLabel, pop: pop,
				roundTrip: fmt.Sprintf("ADD ERROR: %v", err), cgoNote: "yes (C++17)"}
		}
	}

	// Warm up.
	warmQueries := gen.GenerateVectors(nWarmQueries, 7777)
	for _, q := range warmQueries {
		_, _ = idx.Search(q, 10)
	}

	result := eval.MeasureRecallAndLatency(idx, corpus, holdOut)

	// File size.
	savePath := filepath.Join(tmpDir, fmt.Sprintf("usearch_%s_%d.bin", quantLabel, pop))
	_ = idx.Save(savePath)
	sizeKB := fileKB(savePath)

	// Backup round-trip (only at large population).
	rtResult := "N/A"
	if pop == nLarge {
		rtResult = backupRoundTripUsearch(idx, tmpDir, quantLabel, pop)
	}

	return row{
		library:    "usearch",
		quant:      quantLabel,
		pop:        pop,
		recall10:   result.RecallAt10,
		p95ms:      result.P95Ms,
		fileSizeKB: sizeKB,
		roundTrip:  rtResult,
		cgoNote:    "yes (C++17, libusearch_c.so)",
	}
}

func backupRoundTripUsearch(idx *uidx.Index, tmpDir, quantLabel string, pop int) string {
	rtHoldOut := gen.GenerateVectors(nRoundTrip, 5555)
	before := make([][]uint64, nRoundTrip)
	for i, q := range rtHoldOut {
		r, err := idx.Search(q, 10)
		if err != nil {
			return fmt.Sprintf("SEARCH ERROR: %v", err)
		}
		before[i] = r
	}
	path := filepath.Join(tmpDir, fmt.Sprintf("usearch_rt_%s_%d.bin", quantLabel, pop))
	if err := idx.Save(path); err != nil {
		return fmt.Sprintf("SAVE ERROR: %v", err)
	}
	if err := idx.Load(path); err != nil {
		return fmt.Sprintf("LOAD ERROR: %v", err)
	}
	for i, q := range rtHoldOut {
		after, err := idx.Search(q, 10)
		if err != nil {
			return fmt.Sprintf("SEARCH AFTER LOAD ERROR: %v", err)
		}
		if len(before[i]) > 0 && len(after) > 0 && before[i][0] != after[0] {
			return fmt.Sprintf("MISMATCH query %d: before=%d after=%d", i, before[i][0], after[0])
		}
	}
	return "PASS"
}

// evalCohnsw evaluates coder/hnsw at the given population.
func evalCohnsw(tmpDir string, pop int) row {
	corpus := gen.GenerateVectors(pop, 42)
	holdOut := gen.GenerateVectors(nHoldOut, 8888)

	idx := cohnsw.New()
	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			return row{library: "coder/hnsw", quant: "float32", pop: pop,
				roundTrip: fmt.Sprintf("ADD ERROR: %v", err), cgoNote: "none (pure Go)"}
		}
	}

	// Warm up.
	warmQueries := gen.GenerateVectors(nWarmQueries, 7777)
	for _, q := range warmQueries {
		_, _ = idx.Search(q, 10)
	}

	result := eval.MeasureRecallAndLatency(idx, corpus, holdOut)

	savePath := filepath.Join(tmpDir, fmt.Sprintf("cohnsw_%d.bin", pop))
	_ = idx.Save(savePath)
	sizeKB := fileKB(savePath)

	rtResult := "N/A"
	if pop == nLarge {
		rtResult = backupRoundTripCohnsw(idx, tmpDir, pop, corpus)
	}

	return row{
		library:    "coder/hnsw",
		quant:      "float32",
		pop:        pop,
		recall10:   result.RecallAt10,
		p95ms:      result.P95Ms,
		fileSizeKB: sizeKB,
		roundTrip:  rtResult,
		cgoNote:    "none (pure Go)",
	}
}

func backupRoundTripCohnsw(idx *cohnsw.Index, tmpDir string, pop int, _ [][]float32) string {
	rtHoldOut := gen.GenerateVectors(nRoundTrip, 5555)
	before := make([][]uint64, nRoundTrip)
	for i, q := range rtHoldOut {
		r, err := idx.Search(q, 10)
		if err != nil {
			return fmt.Sprintf("SEARCH ERROR: %v", err)
		}
		before[i] = r
	}
	path := filepath.Join(tmpDir, fmt.Sprintf("cohnsw_rt_%d.bin", pop))
	if err := idx.Save(path); err != nil {
		return fmt.Sprintf("SAVE ERROR: %v", err)
	}
	if err := idx.Load(path); err != nil {
		return fmt.Sprintf("LOAD ERROR: %v", err)
	}
	for i, q := range rtHoldOut {
		after, err := idx.Search(q, 10)
		if err != nil {
			return fmt.Sprintf("SEARCH AFTER LOAD ERROR: %v", err)
		}
		if len(before[i]) > 0 && len(after) > 0 && before[i][0] != after[0] {
			return fmt.Sprintf("MISMATCH query %d", i)
		}
	}
	return "PASS"
}

// evalLanceDB evaluates lancedb at the given population.
func evalLanceDB(tmpDir string, pop int) row {
	corpus := gen.GenerateVectors(pop, 42)
	holdOut := gen.GenerateVectors(nHoldOut, 8888)

	dbPath := filepath.Join(tmpDir, fmt.Sprintf("lancedb_%d", pop))
	idx, err := ldb.New(dbPath)
	if err != nil {
		return row{library: "lancedb", quant: "float32", pop: pop,
			roundTrip: fmt.Sprintf("ERROR: %v", err), cgoNote: "yes (Rust FFI)"}
	}
	defer idx.Close()

	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			return row{library: "lancedb", quant: "float32", pop: pop,
				roundTrip: fmt.Sprintf("ADD ERROR: %v", err), cgoNote: "yes (Rust FFI)"}
		}
	}

	// Warm up.
	warmQueries := gen.GenerateVectors(nWarmQueries, 7777)
	for _, q := range warmQueries {
		_, _ = idx.Search(q, 10)
	}

	result := eval.MeasureRecallAndLatency(idx, corpus, holdOut)

	// File size = directory size.
	sizeKB := dirKB(dbPath)

	rtResult := "N/A"
	if pop == nLarge {
		rtResult = backupRoundTripLanceDB(idx, tmpDir, pop)
	}

	return row{
		library:    "lancedb",
		quant:      "float32",
		pop:        pop,
		recall10:   result.RecallAt10,
		p95ms:      result.P95Ms,
		fileSizeKB: sizeKB,
		roundTrip:  rtResult,
		cgoNote:    "yes (Rust FFI, liblancedb_go.a)",
	}
}

func backupRoundTripLanceDB(idx *ldb.Index, tmpDir string, pop int) string {
	rtHoldOut := gen.GenerateVectors(nRoundTrip, 5555)
	before := make([][]uint64, nRoundTrip)
	for i, q := range rtHoldOut {
		r, err := idx.Search(q, 10)
		if err != nil {
			return fmt.Sprintf("SEARCH ERROR: %v", err)
		}
		before[i] = r
	}
	savePath := filepath.Join(tmpDir, fmt.Sprintf("lancedb_rt_%d", pop))
	if err := idx.Save(savePath); err != nil {
		return fmt.Sprintf("SAVE ERROR: %v", err)
	}
	restoredPath := filepath.Join(tmpDir, fmt.Sprintf("lancedb_restored_%d", pop))
	if err := copyDirEval(savePath, restoredPath); err != nil {
		return fmt.Sprintf("COPY ERROR: %v", err)
	}
	if err := idx.Load(restoredPath); err != nil {
		return fmt.Sprintf("LOAD ERROR: %v", err)
	}
	for i, q := range rtHoldOut {
		after, err := idx.Search(q, 10)
		if err != nil {
			return fmt.Sprintf("SEARCH AFTER LOAD ERROR: %v", err)
		}
		if len(before[i]) > 0 && len(after) > 0 && before[i][0] != after[0] {
			return fmt.Sprintf("MISMATCH query %d", i)
		}
	}
	return "PASS"
}

func copyDirEval(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFileEval(path, target)
	})
}

func copyFileEval(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fileKB(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size() / 1024
}

func dirKB(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total / 1024
}

func renderMarkdown(rows []row) string {
	var sb strings.Builder
	sb.WriteString("# HNSW Backing Library Evaluation Results\n\n")
	fmt.Fprintf(&sb, "Generated: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	sb.WriteString("## Evaluation Matrix\n\n")
	sb.WriteString("| Library | Quant | Population | Recall@10 | P95 (ms) | File Size (KB) | Backup Round-Trip | CGo |\n")
	sb.WriteString("|---------|-------|-----------|-----------|----------|---------------|-------------------|-----|\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "| %s | %s | %dk | %.4f | %.1f | %d | %s | %s |\n",
			r.library, r.quant, r.pop/1000,
			r.recall10, r.p95ms, r.fileSizeKB,
			r.roundTrip, r.cgoNote)
	}
	sb.WriteString("\n## DoD Criteria\n\n")
	sb.WriteString("- recall@10 ≥ 0.95 at 50k: check values above\n")
	sb.WriteString("- recall@10 ≥ 0.85 at 250k: check values above\n")
	sb.WriteString("- p95 warm latency ≤ 100ms at k=10 at 250k: check values above\n")
	sb.WriteString("- backup round-trip correctness: PASS/FAIL above\n\n")
	sb.WriteString("## Notes\n\n")
	sb.WriteString("- usearch: CGo, requires libusearch_c.so (v2.25.2, installed from .deb). Supports float32/float16/int8 quantization.\n")
	sb.WriteString("- coder/hnsw: pure Go, no CGo, float32 only. File persistence via Export/Import.\n")
	sb.WriteString("- lancedb: CGo via Rust FFI (liblancedb_go.a). Lance columnar directory format. VectorSearch uses brute-force scan (no explicit HNSW index built in-process in v0.1.2).\n")
	return sb.String()
}
