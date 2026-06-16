package searchcmd

import (
	"path/filepath"
	"testing"
)

// file_path rewriting for ephemeral cache-tier repos turns
// `~/.cache/veska/repos/<sha>/flag.go` into `pflag/flag.go`, but only for
// rows that actually live under that cache dir.
func TestPrettifyEphemeralPaths(t *testing.T) {
	cacheRepoDir := filepath.Join("/home/u/.cache/veska/repos", "3336b2ee32663108e30de97b6c87e528922780c5c73b744e99059049c19f95eb")
	env := SearchEnvelope{Results: []SearchHitView{
		{FilePath: filepath.Join(cacheRepoDir, "flag.go"), LineStart: 194},
		{FilePath: filepath.Join(cacheRepoDir, "golangflag.go"), LineStart: 74},
		{FilePath: "/tmp/junior/greetlib/greet.go", LineStart: 1}, // unrelated row
	}}

	prettifyEphemeralPaths(&env, cacheRepoDir, "pflag")

	if got, want := env.Results[0].FilePath, "pflag/flag.go"; got != want {
		t.Errorf("[0]: got %q want %q", got, want)
	}
	if got, want := env.Results[1].FilePath, "pflag/golangflag.go"; got != want {
		t.Errorf("[1]: got %q want %q", got, want)
	}
	if got, want := env.Results[2].FilePath, "/tmp/junior/greetlib/greet.go"; got != want {
		t.Errorf("[2]: untouched path got %q want %q", got, want)
	}
}

func TestPrettifyEphemeralPaths_Noops(t *testing.T) {
	env := SearchEnvelope{Results: []SearchHitView{{FilePath: "/x/y.go"}}}
	prettifyEphemeralPaths(&env, "", "pflag")
	prettifyEphemeralPaths(&env, "/some/cache", "")
	prettifyEphemeralPaths(nil, "/some/cache", "pflag")
	if env.Results[0].FilePath != "/x/y.go" {
		t.Errorf("noop guards leaked: %q", env.Results[0].FilePath)
	}
}

func TestEphemeralDisplayName(t *testing.T) {
	cases := []struct {
		url, repoID, want string
	}{
		{"https://github.com/spf13/pflag.git", "abc", "pflag"},
		{"https://github.com/spf13/pflag", "abc", "pflag"},
		{"https://github.com/spf13/pflag/", "abc", "pflag"},
		{"", "abcdef0123456789abc", "abcdef012345"},
		{"", "short", "short"},
		{"git@github.com:foo/bar.git", "x", "bar"},
	}
	for _, tc := range cases {
		if got := ephemeralDisplayName(tc.url, tc.repoID); got != tc.want {
			t.Errorf("ephemeralDisplayName(%q,%q) = %q, want %q", tc.url, tc.repoID, got, tc.want)
		}
	}
}
