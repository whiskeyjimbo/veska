// SPDX-License-Identifier: AGPL-3.0-only

package vector

import (
	"fmt"
	"runtime"
)

// usearch build profiles: a single user-facing lever that trades index
// build-speed against recall. Each maps to two underlying dials -
// ef_construction (ExpansionAdd) and build parallelism (BuildThreads).
// Preset (threads, ef) values are calibrated on real repos via
// eval-usearch-profile; the figures below are the measured build time / autolink
// recall on the 113k-node bucket (4-core host), the regime where the lever
// matters (at <=40k nodes every preset is ~instant and recall ~0.997+, so the
// default is fine there). The trade only bites at scale because HNSW recall
// decays as the graph grows - a single ef no longer fits every size, which is
// why the presets exist.
const (
	// ProfileDefault reproduces the historical build exactly: serial, ef64
	// (~25s / recall 0.988 at 113k). The shipping default - the lever is opt-in
	// so enabling a preset never silently changes an existing index. Empty
	// string resolves here too.
	ProfileDefault = "default"
	// ProfileAccurate: deterministic (serial) build with a wide construction
	// beam (~91s / recall 0.9988 at 113k) - highest recall, reproducible graph,
	// slowest. Serial is chosen for determinism, not speed (a parallel ef192
	// reaches similar recall faster but is nondeterministic).
	ProfileAccurate = "accurate"
	// ProfileBalanced: parallel build at a wide beam (~26s / recall ~0.99 at
	// 113k) - about the serial/ef64 wall-clock but recovers the recall ef64
	// loses at scale. The large-repo sweet spot; nondeterministic graph.
	ProfileBalanced = "balanced"
	// ProfileFast: parallel build at the default beam (~12s / recall ~0.98 at
	// 113k) - ~2x faster than the serial default, lowest recall of the three.
	ProfileFast = "fast"
)

// OptionsForProfile resolves a storage usearch_index_profile string into build
// Options. Unknown names are a fail-loud error (mirrors the config contract).
// The default preset is deliberately the historical serial/ef64 build so the
// shipping default is unchanged and enabling a parallel preset is opt-in.
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
