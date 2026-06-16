// Command layercheck enforces architectural layer boundaries in veska packages.
// Usage:
//
//	layercheck [dir]
//
// dir defaults to the current directory (the module root).
// Exits 0 if no violations are found, 1 otherwise.
package main

import (
	"fmt"
	"os"

	"github.com/whiskeyjimbo/veska/tools/lint/layercheck"
)

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}

	violations, err := layercheck.CheckDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "layercheck: %v\n", err)
		os.Exit(2)
	}

	if len(violations) == 0 {
		fmt.Println("layercheck: OK")
		os.Exit(0)
	}

	for _, v := range violations {
		fmt.Fprintln(os.Stderr, v)
	}
	fmt.Fprintf(os.Stderr, "\nlayercheck: %d violation(s) found\n", len(violations))
	os.Exit(1)
}
