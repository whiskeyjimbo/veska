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
