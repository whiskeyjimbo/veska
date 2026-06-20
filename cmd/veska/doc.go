// SPDX-License-Identifier: AGPL-3.0-only

// Package main is the entry point for the veska CLI binary.
// One binary serves three personalities, selected by argv[0]: invoked as
// `veska` it runs the Cobra CLI; the `veska-daemon` and `veska-mcp` symlinks
// dispatch (in main.go) into internal/cli/daemon and internal/cli/mcp
// respectively. The CLI command tree is assembled in newRootCmd.
// Files in this package are Cobra glue: each command's RunE parses flags and
// positionals, builds an options/Params struct, and delegates to the matching
// internal/cli/<name>cmd package where the business logic and its tests live.
// Cross-command helpers (daemon liveness, cwd→repo resolution, byte
// formatting) live in shared.go and are injected into the *cmd packages as
// seams rather than reached for as globals.
package main
