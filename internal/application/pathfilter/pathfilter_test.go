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
