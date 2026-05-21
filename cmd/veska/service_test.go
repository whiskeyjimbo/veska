package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// stubServiceManager is a test double for ServiceManager.
type stubServiceManager struct {
	statusResult ServiceStatus
	statusErr    error
	lastCall     string
}

func (s *stubServiceManager) Install(ctx context.Context) error {
	s.lastCall = "install"
	return nil
}

func (s *stubServiceManager) Uninstall(ctx context.Context) error {
	s.lastCall = "uninstall"
	return nil
}

func (s *stubServiceManager) Start(ctx context.Context) error {
	s.lastCall = "start"
	return nil
}

func (s *stubServiceManager) Stop(ctx context.Context) error {
	s.lastCall = "stop"
	return nil
}

func (s *stubServiceManager) Restart(ctx context.Context) error {
	s.lastCall = "restart"
	return nil
}

func (s *stubServiceManager) Status(ctx context.Context) (ServiceStatus, error) {
	s.lastCall = "status"
	return s.statusResult, s.statusErr
}

func TestServiceCmdName(t *testing.T) {
	cmd := serviceCmd(nil, nil)
	if cmd.Use != "service" {
		t.Errorf("expected Use=service, got %q", cmd.Use)
	}
}

func TestServiceSubcommands(t *testing.T) {
	cmd := serviceCmd(nil, nil)
	want := []string{"install", "uninstall", "start", "stop", "restart", "status"}
	got := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		got[sub.Use] = true
	}
	if len(got) != len(want) {
		t.Errorf("expected %d subcommands, got %d", len(want), len(got))
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestServiceSubcommandsHaveDryRun(t *testing.T) {
	cmd := serviceCmd(nil, nil)
	for _, sub := range cmd.Commands() {
		f := sub.Flags().Lookup("dry-run")
		if f == nil {
			t.Errorf("subcommand %q missing --dry-run flag", sub.Use)
		}
	}
}

func TestServiceStatusWithStub(t *testing.T) {
	stub := &stubServiceManager{
		statusResult: ServiceStatus{
			Running: true,
			PID:     1234,
			Message: "daemon is running",
		},
	}
	cmd := serviceCmd(stub, nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	// Execute the status subcommand.
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "running") && !strings.Contains(out, "1234") && !strings.Contains(out, "daemon") {
		t.Errorf("status output %q does not contain useful information", out)
	}
	if stub.lastCall != "status" {
		t.Errorf("expected stub.lastCall=status, got %q", stub.lastCall)
	}
}

func TestServiceNilManagerReturnsError(t *testing.T) {
	subcommands := []string{"install", "uninstall", "start", "stop", "restart", "status"}
	for _, sub := range subcommands {
		t.Run(sub, func(t *testing.T) {
			cmd := serviceCmd(nil, nil)
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{sub})
			err := cmd.Execute()
			if err == nil {
				t.Errorf("expected error for nil manager on subcommand %q, got nil", sub)
			}
			if !strings.Contains(err.Error(), "service manager not available") {
				t.Errorf("expected 'service manager not available' error, got: %v", err)
			}
		})
	}
}
