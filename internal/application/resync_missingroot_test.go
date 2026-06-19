// SPDX-License-Identifier: AGPL-3.0-only

package application

import (
	"errors"
	"testing"
)

// verification - startup-resync's missing-root predicate
// matches the git CLI's "No such file or directory" wrappings (any case)
// so we downgrade to WARN instead of crying ERROR wolf every boot.

func TestIsMissingRoot(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New(`git [rev-parse HEAD] in /tmp/gone: exit status 128: fatal: cannot change to '/tmp/gone': No such file or directory`), true},
		{errors.New("open /tmp/gone: no such file or directory"), true},
		{errors.New("permission denied"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isMissingRoot(tc.err); got != tc.want {
			t.Errorf("isMissingRoot(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
