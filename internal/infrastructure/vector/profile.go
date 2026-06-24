// SPDX-License-Identifier: AGPL-3.0-only

package vector

import (
	"fmt"
	"runtime"
)

// usearch build profiles: a single user-facing lever that trades index
// build-speed against recall. Each maps to two underlying dials -
// ef_construction (ExpansionAdd) and build parallelism (BuildThreads).
const (
	// ProfileDefault reproduces the historical build exactly: serial, ef64.
	// It is the shipping default until the parallel presets are calibrated on
	// the real graph (eval-usearch-profile); empty string resolves here too.
	ProfileDefault = "default"
	// ProfileAccurate: deterministic (serial) build with a wide construction
	// beam - highest recall, slowest build. Pick on large repos that can spend
	// build time for the best recall.
	ProfileAccurate = "accurate"
	// ProfileBalanced: parallel build at a moderate beam - fast, near-accurate
	// recall, nondeterministic graph.
	ProfileBalanced = "balanced"
	// ProfileFast: parallel build at the default beam - fastest, lowest recall
	// of the three.
	ProfileFast = "fast"
)

// OptionsForProfile resolves a storage usearch_index_profile string into build
// Options. Unknown names are a fail-loud error (mirrors the config contract).
//
// NOTE: the ef/threads values for the parallel presets below are PROVISIONAL -
// placeholders pending calibration on the real graph via eval-usearch-profile.
// The default preset is deliberately the historical serial/ef64
// build so enabling this lever is opt-in and the shipping default is unchanged.
func OptionsForProfile(profile string) ([]Option, error) {
	cores := uint(max(runtime.GOMAXPROCS(0), 1))
	switch profile {
	case "", ProfileDefault:
		return nil, nil
	case ProfileAccurate:
		return []Option{WithExpansionAdd(192), WithBuildThreads(1)}, nil
	case ProfileBalanced:
		return []Option{WithExpansionAdd(128), WithBuildThreads(cores)}, nil
	case ProfileFast:
		return []Option{WithExpansionAdd(64), WithBuildThreads(cores)}, nil
	default:
		return nil, fmt.Errorf("vector: unknown usearch_index_profile %q (want %q, %q, %q, or %q)",
			profile, ProfileDefault, ProfileAccurate, ProfileBalanced, ProfileFast)
	}
}
