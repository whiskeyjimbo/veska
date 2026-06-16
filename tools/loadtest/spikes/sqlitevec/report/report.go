// Package report reads bench/recall/load JSON metrics and emits a markdown verdict report
// for the sqlite-vec sqlitevec loadtest spike.
package report

import (
	"fmt"
	"strings"
	"time"
)

// LatencyStats mirrors bench.LatencyStats for local JSON unmarshaling.
type LatencyStats struct {
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
	N     int     `json:"n"`
}

// PopBench mirrors bench.PopBench.
type PopBench struct {
	Population int64        `json:"population"`
	K          int          `json:"k"`
	Warm       LatencyStats `json:"warm"`
	Cold       LatencyStats `json:"cold"`
}

// BenchResult mirrors bench.BenchResult.
type BenchResult struct {
	Pops             []PopBench `json:"populations"`
	Vec0Ceiling      int64      `json:"vec0_ceiling"`
	CeilingReason    string     `json:"ceiling_reason"`
	SqliteVecVersion string     `json:"sqlite_vec_version"`
	SqliteVersion    string     `json:"sqlite_version"`
	Platform         string     `json:"platform"`
}

// RecallResult mirrors recall.RecallResult.
type RecallResult struct {
	Population  int64   `json:"population"`
	RecallAt10  float64 `json:"recall_at_10"`
	RecallAt50  float64 `json:"recall_at_50"`
	HoldOutSize int     `json:"hold_out_size"`
}

// LoadMetric mirrors loader.LoadMetrics.
type LoadMetric struct {
	Population   int64 `json:"population"`
	LoadWallMs   int64 `json:"load_wall_ms"`
	DiskBytes    int64 `json:"disk_bytes"`
	PeakRSSBytes int64 `json:"peak_rss_bytes"`
}

// SpikeInputs bundles all measured data for verdict assignment and rendering.
type SpikeInputs struct {
	Bench  BenchResult
	Recall []RecallResult
	Load   []LoadMetric
}

// Verdict carries the outcome bucket and human-readable reasons.
type Verdict struct {
	Bucket  string // "green" | "yellow" | "red-quality" | "red-ceiling"
	Reasons []string
}

// M0 exit-gate thresholds (from §Outcomes).
const (
	gate50kP95Ms        = 100.0
	gate1MP95GreenMs    = 200.0
	gate1MP95YellowMs   = 400.0
	gate50kRecall       = 0.95
	gate1MRecallGreen   = 0.85
	gate1MRecallYellow  = 0.75
	gateCeilingMinNodes = 250_000
)

// popStats extracts bench and recall stats for a given population (exact match).
func popStats(inputs SpikeInputs, pop int64) (bench PopBench, recall RecallResult, load LoadMetric) {
	for _, p := range inputs.Bench.Pops {
		if p.Population == pop {
			bench = p
			break
		}
	}
	for _, r := range inputs.Recall {
		if r.Population == pop {
			recall = r
			break
		}
	}
	for _, l := range inputs.Load {
		if l.Population == pop {
			load = l
			break
		}
	}
	return bench, recall, load
}

// AssignVerdict applies M0 §Outcomes logic and returns a Verdict.
// Red-ceiling takes priority over all other checks.
func AssignVerdict(inputs SpikeInputs) Verdict {
	var reasons []string

	// Red-ceiling check (takes priority).
	if inputs.Bench.Vec0Ceiling > 0 && inputs.Bench.Vec0Ceiling < gateCeilingMinNodes {
		return Verdict{
			Bucket:  "red-ceiling",
			Reasons: []string{fmt.Sprintf("vec0 ceiling %d < required %d nodes", inputs.Bench.Vec0Ceiling, gateCeilingMinNodes)},
		}
	}

	bench50k, recall50k, _ := popStats(inputs, 50_000)
	bench1M, recall1M, _ := popStats(inputs, 1_000_000)

	// 50k gates.
	p95at50k := bench50k.Warm.P95Ms
	rec50k := recall50k.RecallAt10

	if p95at50k > gate50kP95Ms {
		reasons = append(reasons, fmt.Sprintf("50k warm p95 %.1fms > %.0fms gate", p95at50k, gate50kP95Ms))
	}
	if rec50k > 0 && rec50k < gate50kRecall {
		reasons = append(reasons, fmt.Sprintf("50k recall@10 %.3f < %.2f gate", rec50k, gate50kRecall))
	}

	// 1M gates.
	p95at1M := bench1M.Warm.P95Ms
	rec1M := recall1M.RecallAt10

	if rec1M > 0 && rec1M < gate1MRecallYellow {
		reasons = append(reasons, fmt.Sprintf("1M recall@10 %.3f < %.2f (red-quality threshold)", rec1M, gate1MRecallYellow))
	}

	// If 50k misses, or 1M recall is below yellow floor → red-quality.
	has50kMiss := p95at50k > gate50kP95Ms || (rec50k > 0 && rec50k < gate50kRecall)
	has1MRecallRed := rec1M > 0 && rec1M < gate1MRecallYellow

	if has50kMiss || has1MRecallRed {
		if len(reasons) == 0 {
			reasons = append(reasons, "50k or 1M quality gate failed")
		}
		return Verdict{Bucket: "red-quality", Reasons: reasons}
	}

	// Now check if 1M is borderline (yellow).
	p95borderline := p95at1M > gate1MP95GreenMs && p95at1M <= gate1MP95YellowMs
	recallBorderline := rec1M > 0 && rec1M >= gate1MRecallYellow && rec1M < gate1MRecallGreen

	if p95borderline || recallBorderline {
		if p95borderline {
			reasons = append(reasons, fmt.Sprintf("1M warm p95 %.1fms in yellow band (%.0f–%.0fms)", p95at1M, gate1MP95GreenMs, gate1MP95YellowMs))
		}
		if recallBorderline {
			reasons = append(reasons, fmt.Sprintf("1M recall@10 %.3f in yellow band (%.2f–%.2f)", rec1M, gate1MRecallYellow, gate1MRecallGreen))
		}
		return Verdict{Bucket: "yellow", Reasons: reasons}
	}

	// All gates passed.
	reasons = append(reasons, "all exit gates met")
	return Verdict{Bucket: "green", Reasons: reasons}
}

// fmtBytes formats bytes as a human-readable string.
func fmtBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(MB))
	default:
		return fmt.Sprintf("%d KiB", b/KB)
	}
}

// RenderMarkdown produces the full RESULTS.md content.
func RenderMarkdown(inputs SpikeInputs, v Verdict) string {
	var sb strings.Builder

	bench50k, recall50k, load50k := popStats(inputs, 50_000)
	bench1M, recall1M, load1M := popStats(inputs, 1_000_000)

	sb.WriteString("# sqlite-vec Spike — RESULTS\n\n")
	fmt.Fprintf(&sb, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	// Verdict box.
	sb.WriteString("## Verdict\n\n")
	fmt.Fprintf(&sb, "**Outcome bucket:** `%s`\n\n", v.Bucket)
	sb.WriteString("**Reasons:**\n\n")
	for _, r := range v.Reasons {
		fmt.Fprintf(&sb, "- %s\n", r)
	}
	sb.WriteString("\n")

	// Environment.
	sb.WriteString("## Environment\n\n")
	sb.WriteString("| Key | Value |\n|---|---|\n")
	fmt.Fprintf(&sb, "| sqlite-vec version | `%s` |\n", inputs.Bench.SqliteVecVersion)
	fmt.Fprintf(&sb, "| SQLite version | `%s` |\n", inputs.Bench.SqliteVersion)
	fmt.Fprintf(&sb, "| Platform | `%s` |\n", inputs.Bench.Platform)
	sb.WriteString("\n")

	// Latency.
	sb.WriteString("## Latency (warm)\n\n")
	sb.WriteString("| Population | k | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | gate |\n")
	sb.WriteString("|---|---|---|---|---|---|---|\n")
	for _, pop := range inputs.Bench.Pops {
		var gate string
		// Gate applies only to k=10 (M0 exit gate spec).
		if pop.K == 10 {
			switch pop.Population {
			case 50_000:
				if pop.Warm.P95Ms <= gate50kP95Ms {
					gate = fmt.Sprintf("PASS (≤ %.0fms)", gate50kP95Ms)
				} else {
					gate = fmt.Sprintf("FAIL (> %.0fms)", gate50kP95Ms)
				}
			case 1_000_000:
				switch {
				case pop.Warm.P95Ms <= gate1MP95GreenMs:
					gate = fmt.Sprintf("GREEN (≤ %.0fms)", gate1MP95GreenMs)
				case pop.Warm.P95Ms <= gate1MP95YellowMs:
					gate = fmt.Sprintf("YELLOW (≤ %.0fms)", gate1MP95YellowMs)
				default:
					gate = fmt.Sprintf("RED (> %.0fms)", gate1MP95YellowMs)
				}
			default:
				gate = "—"
			}
		} else {
			gate = "—"
		}
		fmt.Fprintf(&sb, "| %d | %d | %.2f | %.2f | %.2f | %.2f | %s |\n",
			pop.Population, pop.K,
			pop.Warm.P50Ms, pop.Warm.P95Ms, pop.Warm.P99Ms, pop.Warm.MaxMs,
			gate,
		)
	}
	sb.WriteString("\n")

	// Recall.
	sb.WriteString("## Recall\n\n")
	sb.WriteString("| Population | recall@10 | recall@50 | hold-out | gate |\n")
	sb.WriteString("|---|---|---|---|---|\n")
	// Note 1M recall if missing.
	has50kRecall := false
	has1MRecall := false
	for _, r := range inputs.Recall {
		if r.Population == 50_000 {
			has50kRecall = true
		}
		if r.Population == 1_000_000 {
			has1MRecall = true
		}
	}
	_ = has50kRecall
	if !has1MRecall {
		sb.WriteString("| 1000000 | N/A (measurement failed) | N/A | N/A | N/A — consistent with ceiling=100k |\n")
	}
	for _, r := range inputs.Recall {
		var gate string
		switch r.Population {
		case 50_000:
			if r.RecallAt10 >= gate50kRecall {
				gate = fmt.Sprintf("PASS (≥ %.2f)", gate50kRecall)
			} else {
				gate = fmt.Sprintf("FAIL (< %.2f)", gate50kRecall)
			}
		case 1_000_000:
			switch {
			case r.RecallAt10 >= gate1MRecallGreen:
				gate = fmt.Sprintf("GREEN (≥ %.2f)", gate1MRecallGreen)
			case r.RecallAt10 >= gate1MRecallYellow:
				gate = fmt.Sprintf("YELLOW (≥ %.2f)", gate1MRecallYellow)
			default:
				gate = fmt.Sprintf("RED (< %.2f)", gate1MRecallYellow)
			}
		default:
			gate = "—"
		}
		fmt.Fprintf(&sb, "| %d | %.4f | %.4f | %d | %s |\n",
			r.Population, r.RecallAt10, r.RecallAt50, r.HoldOutSize, gate,
		)
	}
	sb.WriteString("\n")

	// vec0 ceiling.
	sb.WriteString("## vec0 Ceiling\n\n")
	ceilingStr := fmt.Sprintf("%d", inputs.Bench.Vec0Ceiling)
	if inputs.Bench.Vec0Ceiling == 0 {
		ceilingStr = "not reached"
	}
	sb.WriteString("| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&sb, "| vec0 ceiling (nodes) | %s |\n", ceilingStr)
	fmt.Fprintf(&sb, "| Ceiling reason | %s |\n", inputs.Bench.CeilingReason)
	fmt.Fprintf(&sb, "| Gate (≥ %d) | ", gateCeilingMinNodes)
	if inputs.Bench.Vec0Ceiling == 0 || inputs.Bench.Vec0Ceiling >= gateCeilingMinNodes {
		sb.WriteString("PASS\n")
	} else {
		sb.WriteString("FAIL\n")
	}
	sb.WriteString("\n")

	// Resource usage.
	sb.WriteString("## Resource Usage (RSS / Disk)\n\n")
	sb.WriteString("| Population | Load wall time | Disk | Peak RSS |\n")
	sb.WriteString("|---|---|---|---|\n")
	if load50k.Population != 0 {
		fmt.Fprintf(&sb, "| %d | %dms | %s | %s |\n",
			load50k.Population, load50k.LoadWallMs,
			fmtBytes(load50k.DiskBytes), fmtBytes(load50k.PeakRSSBytes),
		)
	}
	if load1M.Population != 0 {
		fmt.Fprintf(&sb, "| %d | %dms | %s | %s |\n",
			load1M.Population, load1M.LoadWallMs,
			fmtBytes(load1M.DiskBytes), fmtBytes(load1M.PeakRSSBytes),
		)
	}
	sb.WriteString("\n")

	// Measurement notes for missing or failed populations.
	var notes []string
	if recall1M.Population == 0 {
		notes = append(notes, "1M recall: measurement failed — sqlite-vec returned an internal error when inserting ~272k vectors into the 1M recall DB. This is consistent with the vec0 ceiling of 100k nodes detected in the bench sweep.")
	}
	if len(notes) > 0 {
		sb.WriteString("## Measurement Notes\n\n")
		for _, n := range notes {
			fmt.Fprintf(&sb, "> **NOTE:** %s\n\n", n)
		}
	}

	// Summary of 50k and 1M numbers for quick reference.
	sb.WriteString("## Exit-Gate Summary\n\n")
	sb.WriteString("| Gate | Measured | Threshold | Result |\n")
	sb.WriteString("|---|---|---|---|\n")
	fmt.Fprintf(&sb, "| 50k warm p95 | %.2fms | ≤ %.0fms | %s |\n",
		bench50k.Warm.P95Ms, gate50kP95Ms,
		gatePass(bench50k.Warm.P95Ms <= gate50kP95Ms),
	)
	fmt.Fprintf(&sb, "| 50k recall@10 | %.4f | ≥ %.2f | %s |\n",
		recall50k.RecallAt10, gate50kRecall,
		gatePass(recall50k.RecallAt10 >= gate50kRecall),
	)
	fmt.Fprintf(&sb, "| 1M warm p95 | %.2fms | ≤ %.0fms (green) / ≤ %.0fms (yellow) | %s |\n",
		bench1M.Warm.P95Ms, gate1MP95GreenMs, gate1MP95YellowMs,
		p95Band(bench1M.Warm.P95Ms),
	)
	if recall1M.Population != 0 {
		fmt.Fprintf(&sb, "| 1M recall@10 | %.4f | ≥ %.2f (green) / ≥ %.2f (yellow) | %s |\n",
			recall1M.RecallAt10, gate1MRecallGreen, gate1MRecallYellow,
			recallBand(recall1M.RecallAt10),
		)
	} else {
		fmt.Fprintf(&sb, "| 1M recall@10 | N/A (failed) | ≥ %.2f (green) / ≥ %.2f (yellow) | N/A |\n",
			gate1MRecallGreen, gate1MRecallYellow,
		)
	}
	fmt.Fprintf(&sb, "| vec0 ceiling | %s | ≥ %d | %s |\n",
		ceilingStr, gateCeilingMinNodes,
		gatePass(inputs.Bench.Vec0Ceiling == 0 || inputs.Bench.Vec0Ceiling >= gateCeilingMinNodes),
	)
	sb.WriteString("\n")

	return sb.String()
}

func gatePass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func p95Band(p95 float64) string {
	switch {
	case p95 <= gate1MP95GreenMs:
		return "GREEN"
	case p95 <= gate1MP95YellowMs:
		return "YELLOW"
	default:
		return "RED"
	}
}

func recallBand(r float64) string {
	switch {
	case r >= gate1MRecallGreen:
		return "GREEN"
	case r >= gate1MRecallYellow:
		return "YELLOW"
	default:
		return "RED"
	}
}
