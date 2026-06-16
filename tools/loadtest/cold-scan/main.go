//go:build cold_scan

// Command cold-scan benchmarks cold parsing of a synthetic ~100k LOC Go codebase
// using GoParser. It generates 1000 synthetic Go source files (~100 LOC each),
// parses each with GoParser.ParseFile, records per-file latency, and writes
// RESULTS.md. Exits non-zero if total elapsed > 60s.
// Usage:
//
//	go build -tags cold_scan -o /tmp/cold-scan./tools/loadtest/cold-scan/
//	/tmp/cold-scan
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

const (
	numFiles    = 1000
	gateSeconds = 60
	resultsPath = "tools/loadtest/cold-scan/RESULTS.md"
)

// synthFile generates ~100 lines of valid Go source for file index i.
func synthFile(i int) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "package synth\n\nimport \"fmt\"\n\n")

	// 5 struct+constructor+method groups → ~20 lines each → ~100 LOC
	for g := 0; g < 5; g++ {
		name := fmt.Sprintf("T%05d%02d", i, g)
		fmt.Fprintf(&sb, "// %s is a synthetic type generated for benchmark file %d group %d.\n", name, i, g)
		fmt.Fprintf(&sb, "type %s struct {\n\tID   int\n\tName string\n\tVal  float64\n\tTag  string\n}\n\n", name)
		fmt.Fprintf(&sb, "// New%s constructs a %s.\n", name, name)
		fmt.Fprintf(&sb, "func New%s(id int, name string, val float64, tag string) *%s {\n", name, name)
		fmt.Fprintf(&sb, "\treturn &%s{ID: id, Name: name, Val: val, Tag: tag}\n}\n\n", name)
		fmt.Fprintf(&sb, "// String implements fmt.Stringer for %s.\n", name)
		fmt.Fprintf(&sb, "func (x *%s) String() string {\n", name)
		fmt.Fprintf(&sb, "\treturn fmt.Sprintf(\"%s{%%d %%s %%f %%s}\", x.ID, x.Name, x.Val, x.Tag)\n}\n\n", name)
		fmt.Fprintf(&sb, "// Validate checks that %s fields are non-zero.\n", name)
		fmt.Fprintf(&sb, "func (x *%s) Validate() bool {\n\treturn x.ID > 0 && x.Name != \"\" && x.Tag != \"\"\n}\n\n", name)
	}

	return []byte(sb.String())
}

func main() {
	ctx := context.Background()
	parser := treesitter.NewGoParser()

	// ── generate synthetic files ─────────────────────────────────────────────
	tmpDir, err := os.MkdirTemp("", "cold-scan-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdirtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("Generating %d synthetic Go files in %s ...\n", numFiles, tmpDir)
	var totalLOC int
	files := make([]string, 0, numFiles)
	for i := 0; i < numFiles; i++ {
		src := synthFile(i)
		totalLOC += countLines(src)
		path := filepath.Join(tmpDir, fmt.Sprintf("synth_%05d.go", i))
		if err := os.WriteFile(path, src, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
			os.Exit(1)
		}
		files = append(files, path)
	}
	fmt.Printf("Generated %d files, %d total LOC\n", numFiles, totalLOC)

	// ── parse all files ───────────────────────────────────────────────────────
	fmt.Printf("Parsing with GoParser ...\n")
	latencies := make([]time.Duration, 0, numFiles)
	wallStart := time.Now()

	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
			os.Exit(1)
		}
		t0 := time.Now()
		_, err = parser.ParseFile(ctx, "bench-repo", path, src)
		latencies = append(latencies, time.Since(t0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse %s: %v\n", path, err)
			os.Exit(1)
		}
	}

	totalElapsed := time.Since(wallStart)

	// ── statistics ────────────────────────────────────────────────────────────
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[int(float64(len(latencies))*0.95)]
	filesPerSec := float64(numFiles) / totalElapsed.Seconds()

	gate := "PASS"
	exitCode := 0
	if totalElapsed.Seconds() > gateSeconds {
		gate = "FAIL"
		exitCode = 1
	}

	fmt.Printf("\n=== Cold-Scan Results ===\n")
	fmt.Printf("Files:         %d\n", numFiles)
	fmt.Printf("Total LOC:     ~%dk\n", totalLOC/1000)
	fmt.Printf("Total elapsed: %s\n", totalElapsed.Round(time.Millisecond))
	fmt.Printf("p95 per-file:  %s\n", p95.Round(time.Microsecond))
	fmt.Printf("Files/sec:     %.0f\n", filesPerSec)
	fmt.Printf("Gate (<60s):   %s\n", gate)

	// ── write RESULTS.md ──────────────────────────────────────────────────────
	if err := writeResults(totalElapsed, p95, filesPerSec, numFiles, totalLOC, gate); err != nil {
		fmt.Fprintf(os.Stderr, "write results: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nResults written to %s\n", resultsPath)

	os.Exit(exitCode)
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

const resultsTmpl = `# Cold-Scan Benchmark — GoParser

Generated: {{.Date}}
Platform: {{.OS}} {{.Arch}}
Files: {{.Files}}
Total LOC: ~{{.LOCk}}k
Gate: <60s total elapsed

| Metric | Value | Gate |
|--------|-------|------|
| Total elapsed | {{.TotalElapsed}} | <60s |
| Per-file p95 | {{.P95}} | — |
| Files/sec | {{.FilesPerSec}} | — |

Gate: {{.GateVerdict}}
`

type resultsData struct {
	Date         string
	OS           string
	Arch         string
	Files        int
	LOCk         int
	TotalElapsed string
	P95          string
	FilesPerSec  string
	GateVerdict  string
}

func writeResults(total, p95 time.Duration, fps float64, files, loc int, gate string) error {
	data := resultsData{
		Date:         time.Now().UTC().Format("2006-01-02"),
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Files:        files,
		LOCk:         loc / 1000,
		TotalElapsed: total.Round(time.Millisecond).String(),
		P95:          p95.Round(time.Microsecond).String(),
		FilesPerSec:  fmt.Sprintf("%.0f", fps),
		GateVerdict:  gate,
	}

	t := template.Must(template.New("results").Parse(resultsTmpl))
	f, err := os.Create(resultsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return t.Execute(f, data)
}
