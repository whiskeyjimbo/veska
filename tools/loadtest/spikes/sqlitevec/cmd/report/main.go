// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Command report reads bench/recall/load JSON metrics and writes RESULTS.md with a verdict.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/report"
)

func main() {
	benchFile := flag.String("bench", "data/bench_metrics.json", "path to bench_metrics.json")
	recallFile := flag.String("recall", "data/recall_metrics.json", "path to recall_metrics.json")
	loadFile := flag.String("load", "data/load_metrics.json", "path to load_metrics.json")
	outFile := flag.String("out", "RESULTS.md", "output path for RESULTS.md")
	flag.Parse()

	var inputs report.SpikeInputs

	if err := readJSON(*benchFile, &inputs.Bench); err != nil {
		fmt.Fprintf(os.Stderr, "report: reading bench file: %v\n", err)
		os.Exit(1)
	}
	if err := readJSON(*recallFile, &inputs.Recall); err != nil {
		fmt.Fprintf(os.Stderr, "report: reading recall file: %v\n", err)
		os.Exit(1)
	}
	if err := readJSON(*loadFile, &inputs.Load); err != nil {
		fmt.Fprintf(os.Stderr, "report: reading load file: %v\n", err)
		os.Exit(1)
	}

	v := report.AssignVerdict(inputs)
	md := report.RenderMarkdown(inputs, v)

	if err := os.WriteFile(*outFile, []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "report: writing output: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("verdict: %s\n", v.Bucket)
	fmt.Printf("wrote: %s\n", *outFile)
}

func readJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
