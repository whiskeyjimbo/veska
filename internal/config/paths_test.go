package config_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/config"
)

func TestDefaultVectorDir_NonEmpty(t *testing.T) {
	dir := config.DefaultVectorDir()
	if dir == "" {
		t.Fatal("DefaultVectorDir() returned empty string")
	}
}

func TestDefaultVectorDir_ContainsDotVeska(t *testing.T) {
	dir := config.DefaultVectorDir()
	if !strings.Contains(dir, ".veska") {
		t.Errorf("DefaultVectorDir() = %q; want path containing \".veska\"", dir)
	}
}

func TestCLISockPath_EndsWithCLISock(t *testing.T) {
	got := config.CLISockPath()
	if !strings.HasSuffix(got, "cli.sock") {
		t.Errorf("CLISockPath() = %q; want path ending in \"cli.sock\"", got)
	}
}

func TestCLISockPath_RespectsVeskaHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	got := config.CLISockPath()
	want := dir + "/cli.sock"
	if got != want {
		t.Errorf("CLISockPath() = %q; want %q", got, want)
	}
}

func TestMCPSockPath_EndsWithMCPSock(t *testing.T) {
	got := config.MCPSockPath()
	if !strings.HasSuffix(got, "mcp.sock") {
		t.Errorf("MCPSockPath() = %q; want path ending in \"mcp.sock\"", got)
	}
}

func TestMCPSockPath_RespectsVeskaHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	got := config.MCPSockPath()
	want := dir + "/mcp.sock"
	if got != want {
		t.Errorf("MCPSockPath() = %q; want %q", got, want)
	}
}

func TestCacheDir_PrecedenceTable(t *testing.T) {
	cases := []struct {
		name           string
		veskaCacheHome string
		xdgCacheHome   string
		wantSuffix     string
		wantExact      string
	}{
		{
			name:           "VESKA_CACHE_HOME wins over XDG_CACHE_HOME",
			veskaCacheHome: "/explicit/cache",
			xdgCacheHome:   "/should/be/ignored",
			wantExact:      "/explicit/cache",
		},
		{
			name:         "XDG_CACHE_HOME used when VESKA_CACHE_HOME unset",
			xdgCacheHome: "/xdg/cache",
			wantExact:    "/xdg/cache/veska",
		},
		{
			name:       "fallback to ~/.cache/veska when both unset",
			wantSuffix: "/.cache/veska",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VESKA_CACHE_HOME", tc.veskaCacheHome)
			t.Setenv("XDG_CACHE_HOME", tc.xdgCacheHome)
			got := config.CacheDir()
			if tc.wantExact != "" && got != tc.wantExact {
				t.Errorf("CacheDir() = %q; want %q", got, tc.wantExact)
			}
			if tc.wantSuffix != "" && !strings.HasSuffix(got, tc.wantSuffix) {
				t.Errorf("CacheDir() = %q; want suffix %q", got, tc.wantSuffix)
			}
		})
	}
}

func TestRepoCachePath_UnderCacheDirRepos(t *testing.T) {
	t.Setenv("VESKA_CACHE_HOME", "/cache/root")
	t.Setenv("XDG_CACHE_HOME", "")
	got := config.RepoCachePath("abc123")
	want := "/cache/root/repos/abc123"
	if got != want {
		t.Errorf("RepoCachePath(abc123) = %q; want %q", got, want)
	}
}
