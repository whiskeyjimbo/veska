// SPDX-License-Identifier: AGPL-3.0-only

package vector

import (
	"runtime"
	"testing"
)

// TestOptionsForProfile pins the speed-vs-accuracy lever's mapping: each named
// profile resolves to the calibrated (ef_construction, build_threads) dials, and
// an unknown name fails loud.
func TestOptionsForProfile(t *testing.T) {
	cores := uint(max(runtime.GOMAXPROCS(0), 1))

	resolve := func(t *testing.T, profile string) Options {
		t.Helper()
		opts, err := OptionsForProfile(profile)
		if err != nil {
			t.Fatalf("OptionsForProfile(%q): %v", profile, err)
		}
		var o Options
		for _, fn := range opts {
			fn(&o)
		}
		return o
	}

	t.Run("default and empty are the historical serial/ef64 build (zero Options)", func(t *testing.T) {
		for _, p := range []string{"", ProfileDefault} {
			o := resolve(t, p)
			if o.ExpansionAdd != 0 || o.BuildThreads != 0 {
				t.Errorf("profile %q = %+v, want zero Options (package defaults)", p, o)
			}
		}
	})

	t.Run("accurate is deterministic (serial) with a wide beam", func(t *testing.T) {
		o := resolve(t, ProfileAccurate)
		if o.ExpansionAdd != 192 || o.BuildThreads != 1 {
			t.Errorf("accurate = %+v, want ef=192 threads=1", o)
		}
	})

	t.Run("balanced is parallel at a wide beam", func(t *testing.T) {
		o := resolve(t, ProfileBalanced)
		if o.ExpansionAdd != 128 || o.BuildThreads != cores {
			t.Errorf("balanced = %+v, want ef=128 threads=%d", o, cores)
		}
	})

	t.Run("fast is parallel at the default beam", func(t *testing.T) {
		o := resolve(t, ProfileFast)
		if o.ExpansionAdd != 64 || o.BuildThreads != cores {
			t.Errorf("fast = %+v, want ef=64 threads=%d", o, cores)
		}
	})

	t.Run("unknown profile is a fail-loud error", func(t *testing.T) {
		if _, err := OptionsForProfile("turbo"); err == nil {
			t.Fatal("OptionsForProfile(\"turbo\") = nil error, want fail-loud")
		}
	})
}
