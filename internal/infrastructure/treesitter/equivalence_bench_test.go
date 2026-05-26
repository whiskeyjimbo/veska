package treesitter_test

// equivalence_bench_test.go baselines parse throughput for the legacy
// walker vs the query-driven path on a non-trivial Go file (solov2-1yev).
//
// Phase 1 numbers cover only function-declaration extraction — the
// query parser doesn't emit methods/types/calls/imports yet, so the
// "fewer CGO crossings" claim is provisional until later phases land.
// Re-run with `go test -bench=. -benchmem ./internal/infrastructure/treesitter/`.

import (
	"context"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// benchFixture is a moderately-sized synthetic Go file: 200 top-level
// function declarations with varying signature shapes. Large enough
// that per-node CGO crossings dominate over per-file fixed costs;
// small enough to stay deterministic across runs.
var benchFixture = func() []byte {
	var b strings.Builder
	b.WriteString("package bench\n\n")
	for i := 0; i < 200; i++ {
		b.WriteString("// Generated function.\n")
		b.WriteString("func F")
		b.WriteString(itoa(i))
		b.WriteString("(x int, opts map[string]int) (string, error) {\n")
		b.WriteString("\treturn \"\", nil\n")
		b.WriteString("}\n\n")
	}
	return []byte(b.String())
}()

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func BenchmarkParseFile_LegacyGoParser(b *testing.B) {
	p := treesitter.NewGoParser()
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := p.ParseFile(ctx, "repo", "bench.go", benchFixture)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
	}
}

func BenchmarkParseFile_QueryGoParser(b *testing.B) {
	p := treesitter.NewGoQueryParser()
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := p.ParseFile(ctx, "repo", "bench.go", benchFixture)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
	}
}
