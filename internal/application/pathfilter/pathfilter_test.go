// SPDX-License-Identifier: AGPL-3.0-only

package pathfilter

import "testing"

func TestIsVendored(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"", false},
		{"cmd/main.go", false},
		{"vendor/github.com/spf13/cobra/cobra.go", true},
		{"node_modules/lodash/index.js", true},
		{"third_party/protobuf/x.proto", true},
		{"apps/cli/vendor/github.com/spf13/pflag/x.go", true},
		{"services/api/node_modules/express/index.js", true},
		{"bower_components/jquery/jquery.js", true},
		{"jspm_packages/npm/lodash/index.js", true},
		// Substrings must not match.
		{"vendored_data/keys.txt", false},
		{"my_vendor.go", false},
		{"node_modules_backup/index.js", false},
	}
	for _, c := range cases {
		if got := IsVendored(c.path); got != c.want {
			t.Errorf("IsVendored(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"", false},
		{"internal/svc/svc.go", false},
		{"internal/svc/svc_test.go", true},   // Go
		{"pkg/handler_test.go", true},        // Go nested
		{"tests/test_thing.py", true},        // pytest prefix
		{"app/thing_test.py", true},          // pytest suffix
		{"src/component.test.ts", true},      // jest/vitest
		{"src/component.test.tsx", true},     // jest tsx
		{"src/component.spec.js", true},      // jasmine/jest
		{"src/component.spec.jsx", true},     // jasmine jsx
		{"src/component.ts", false},          // not a test
		{"thing_test.rb", false},             // unsupported lang
		{"my_test_helpers.go", false},        // not the _test.go suffix
		{"C:\\proj\\pkg\\svc_test.go", true}, // windows separator
	}
	for _, c := range cases {
		if got := IsTestFile(c.path); got != c.want {
			t.Errorf("IsTestFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
