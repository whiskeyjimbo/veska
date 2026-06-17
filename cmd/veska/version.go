// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// resolveVersion returns the module version string, falling back to "dev" when
// the binary was built without a tagged version (go run / `go build` of a
// working tree both report "" or "(devel)"). Single source of truth for both
// the --version flag template and the `version` subcommand.
func resolveVersion(info *debug.BuildInfo) string {
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		return "dev"
	}
	return v
}

// shortVersion returns just the module version string for use with cobra's
// version flag template.
func shortVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return resolveVersion(info)
}

// versionCmd prints the binary's build info - module version, VCS revision,
// commit time, and Go runtime - using runtime/debug.ReadBuildInfo so no
// ldflag-set version variable is needed.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print veska version and build info",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			info, ok := debug.ReadBuildInfo()
			if !ok {
				fmt.Fprintln(out, "veska (build info unavailable)")
				return nil
			}
			version := resolveVersion(info)
			var rev, when, modified string
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					rev = s.Value
				case "vcs.time":
					when = s.Value
				case "vcs.modified":
					modified = s.Value
				}
			}
			fmt.Fprintf(out, "veska %s\n", version)
			if rev != "" {
				dirty := ""
				if modified == "true" {
					dirty = " (dirty)"
				}
				fmt.Fprintf(out, "commit:  %s%s\n", rev, dirty)
			}
			if when != "" {
				fmt.Fprintf(out, "built:   %s\n", when)
			}
			fmt.Fprintf(out, "go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
