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

func TestDefaultVectorDir_ContainsDotEngram(t *testing.T) {
	dir := config.DefaultVectorDir()
	if !strings.Contains(dir, ".veska") {
		t.Errorf("DefaultVectorDir() = %q; want path containing \".veska\"", dir)
	}
}

func TestDaemonSockPath_EndsWithDaemonSock(t *testing.T) {
	got := config.DaemonSockPath()
	if !strings.HasSuffix(got, "daemon.sock") {
		t.Errorf("DaemonSockPath() = %q; want path ending in \"daemon.sock\"", got)
	}
}

func TestDaemonSockPath_RespectsEngramHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	got := config.DaemonSockPath()
	want := dir + "/daemon.sock"
	if got != want {
		t.Errorf("DaemonSockPath() = %q; want %q", got, want)
	}
}

func TestMCPSockPath_EndsWithMCPSock(t *testing.T) {
	got := config.MCPSockPath()
	if !strings.HasSuffix(got, "mcp.sock") {
		t.Errorf("MCPSockPath() = %q; want path ending in \"mcp.sock\"", got)
	}
}

func TestMCPSockPath_RespectsEngramHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	got := config.MCPSockPath()
	want := dir + "/mcp.sock"
	if got != want {
		t.Errorf("MCPSockPath() = %q; want %q", got, want)
	}
}
