package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/service"
)

func TestNewDarwin(t *testing.T) {
	mgr, err := service.NewForGOOS("darwin", "/bin/veska-daemon", "/home/user/.veska")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := mgr.(*service.LaunchdManager); !ok {
		t.Fatalf("expected *LaunchdManager, got %T", mgr)
	}
}

func TestNewLinux(t *testing.T) {
	mgr, err := service.NewForGOOS("linux", "/bin/veska-daemon", "/home/user/.veska")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := mgr.(*service.SystemdManager); !ok {
		t.Fatalf("expected *SystemdManager, got %T", mgr)
	}
}

func TestNewUnsupported(t *testing.T) {
	_, err := service.NewForGOOS("windows", "/bin/veska-daemon", "/home/user/.veska")
	if err == nil {
		t.Fatal("expected error for unsupported platform, got nil")
		return
	}
}

func TestLaunchdPlistRender(t *testing.T) {
	out, err := service.RenderPlist("/usr/local/bin/veska-daemon", "/home/user/.veska")
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	if !strings.Contains(out, "/usr/local/bin/veska-daemon") {
		t.Error("plist output missing binary path")
	}
	if !strings.Contains(out, "com.veska.daemon") {
		t.Error("plist output missing service label")
	}
}

func TestSystemdUnitRender(t *testing.T) {
	out, err := service.RenderUnit("/usr/local/bin/veska-daemon", "/home/user/.veska")
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	if !strings.Contains(out, "[Service]") {
		t.Error("unit output missing [Service] section")
	}
	if !strings.Contains(out, "/usr/local/bin/veska-daemon") {
		t.Error("unit output missing binary path")
	}
}

func TestLaunchdInstallDryRun(t *testing.T) {
	mgr := service.NewLaunchdManager("/bin/veska-daemon", "/home/user/.veska", true)
	if err := mgr.Install(context.Background()); err != nil {
		t.Fatalf("dry-run Install: %v", err)
	}
}

func TestSystemdInstallDryRun(t *testing.T) {
	mgr := service.NewSystemdManager("/bin/veska-daemon", "/home/user/.veska", true)
	if err := mgr.Install(context.Background()); err != nil {
		t.Fatalf("dry-run Install: %v", err)
	}
}
