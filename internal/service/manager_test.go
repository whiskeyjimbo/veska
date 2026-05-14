package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/internal/service"
)

func TestNewDarwin(t *testing.T) {
	mgr, err := service.NewForGOOS("darwin", "/bin/engram-daemon", "/home/user/.engram")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := mgr.(*service.LaunchdManager); !ok {
		t.Fatalf("expected *LaunchdManager, got %T", mgr)
	}
}

func TestNewLinux(t *testing.T) {
	mgr, err := service.NewForGOOS("linux", "/bin/engram-daemon", "/home/user/.engram")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := mgr.(*service.SystemdManager); !ok {
		t.Fatalf("expected *SystemdManager, got %T", mgr)
	}
}

func TestNewUnsupported(t *testing.T) {
	_, err := service.NewForGOOS("windows", "/bin/engram-daemon", "/home/user/.engram")
	if err == nil {
		t.Fatal("expected error for unsupported platform, got nil")
	}
}

func TestLaunchdPlistRender(t *testing.T) {
	out, err := service.RenderPlist("/usr/local/bin/engram-daemon", "/home/user/.engram")
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	if !strings.Contains(out, "/usr/local/bin/engram-daemon") {
		t.Error("plist output missing binary path")
	}
	if !strings.Contains(out, "com.engram.daemon") {
		t.Error("plist output missing service label")
	}
}

func TestSystemdUnitRender(t *testing.T) {
	out, err := service.RenderUnit("/usr/local/bin/engram-daemon", "/home/user/.engram")
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	if !strings.Contains(out, "[Service]") {
		t.Error("unit output missing [Service] section")
	}
	if !strings.Contains(out, "/usr/local/bin/engram-daemon") {
		t.Error("unit output missing binary path")
	}
}

func TestLaunchdInstallDryRun(t *testing.T) {
	mgr := service.NewLaunchdManager("/bin/engram-daemon", "/home/user/.engram", true)
	if err := mgr.Install(context.Background()); err != nil {
		t.Fatalf("dry-run Install: %v", err)
	}
}

func TestSystemdInstallDryRun(t *testing.T) {
	mgr := service.NewSystemdManager("/bin/engram-daemon", "/home/user/.engram", true)
	if err := mgr.Install(context.Background()); err != nil {
		t.Fatalf("dry-run Install: %v", err)
	}
}
