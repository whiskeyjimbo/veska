// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Command fatfiles enforces the per-file LOC ratchet.
// Usage:
//
//	fatfiles [-root dir] [-inventory path]
//
// It reads the checked-in inventory of oversized files (default
// tools/lint/fatfiles/inventory.txt), measures each entry's current line count,
// and exits non-zero if any file has grown past its recorded ceiling (or is
// inventoried but missing). The ratchet only ratchets DOWN: shrinking a file and
// lowering its recorded value keeps the gate green.
// To update an entry after thinning a file, run the file's new `wc -l` and set
// the inventory record to that value (never raise it).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/whiskeyjimbo/veska/tools/lint/fatfiles"
)

func main() {
	root := flag.String("root", ".", "module root to resolve inventoried paths against")
	inventory := flag.String("inventory", "tools/lint/fatfiles/inventory.txt", "path to the fat-file inventory")
	flag.Parse()

	violations, err := fatfiles.CheckDir(*root, *inventory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatfiles: %v\n", err)
		os.Exit(2)
	}

	if len(violations) == 0 {
		fmt.Println("fatfiles: OK")
		os.Exit(0)
	}

	for _, v := range violations {
		fmt.Fprintln(os.Stderr, v)
	}
	fmt.Fprintf(os.Stderr, "\nfatfiles: %d violation(s) - the LOC ratchet only ratchets down; shrink the file (WS-2) or, if you legitimately shrank it, lower its inventory entry\n", len(violations))
	os.Exit(1)
}
