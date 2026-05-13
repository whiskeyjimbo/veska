package config_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/internal/config"
)

func TestDefaultVectorDir_NonEmpty(t *testing.T) {
	dir := config.DefaultVectorDir()
	if dir == "" {
		t.Fatal("DefaultVectorDir() returned empty string")
	}
}

func TestDefaultVectorDir_ContainsDotEngram(t *testing.T) {
	dir := config.DefaultVectorDir()
	if !strings.Contains(dir, ".engram") {
		t.Errorf("DefaultVectorDir() = %q; want path containing \".engram\"", dir)
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
	t.Setenv("ENGRAM_HOME", dir)
	got := config.DaemonSockPath()
	want := dir + "/daemon.sock"
	if got != want {
		t.Errorf("DaemonSockPath() = %q; want %q", got, want)
	}
}
