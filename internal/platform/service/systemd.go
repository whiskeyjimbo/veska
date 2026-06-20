// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

const systemdUnit = "veska-daemon"

// unitTemplateText is the embedded systemd unit template.
const unitTemplateText = `[Unit]
Description=Veska code intelligence daemon
After=network.target

[Service]
ExecStart={{.BinaryPath}}
Restart=on-failure
RestartSec=5
Environment=VESKA_HOME={{.VeskaHome}}
StandardOutput=append:{{.VeskaHome}}/daemon.log
StandardError=append:{{.VeskaHome}}/daemon.log

[Install]
WantedBy=default.target
`

// SystemdManager manages the veska daemon via systemd --user on Linux.
type SystemdManager struct {
	binaryPath string
	veskaHome  string
	dryRun     bool
}

// When dryRun is true, mutating operations print what they would do.
func NewSystemdManager(binaryPath, veskaHome string, dryRun bool) *SystemdManager {
	return &SystemdManager{
		binaryPath: binaryPath,
		veskaHome:  veskaHome,
		dryRun:     dryRun,
	}
}

// unitPath returns the systemd user unit file path.
func (m *SystemdManager) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("systemd: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnit+".service"), nil
}

// RenderUnit renders the systemd unit template for the given binary and home.
// Exported for testing.
func RenderUnit(binaryPath, veskaHome string) (string, error) {
	tmpl, err := template.New("unit").Parse(unitTemplateText)
	if err != nil {
		return "", fmt.Errorf("systemd: parse unit template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		BinaryPath string
		VeskaHome  string
	}{binaryPath, veskaHome}); err != nil {
		return "", fmt.Errorf("systemd: render unit: %w", err)
	}
	return buf.String(), nil
}

// Install writes the unit file, then runs daemon-reload + enable.
func (m *SystemdManager) Install(ctx context.Context) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	content, err := RenderUnit(m.binaryPath, m.veskaHome)
	if err != nil {
		return err
	}
	if m.dryRun {
		fmt.Printf("dry-run: would write unit to %s\n", path)
		fmt.Printf("dry-run: would run: systemctl --user daemon-reload\n")
		fmt.Printf("dry-run: would run: systemctl --user enable %s\n", systemdUnit)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("systemd: create unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("systemd: write unit: %w", err)
	}
	if err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return m.run(ctx, "systemctl", "--user", "enable", systemdUnit)
}

// Uninstall disables the unit, removes the file, and reloads.
func (m *SystemdManager) Uninstall(ctx context.Context) error {
	path, err := m.unitPath()
	if err != nil {
		return err
	}
	if m.dryRun {
		fmt.Printf("dry-run: would run: systemctl --user disable %s\n", systemdUnit)
		fmt.Printf("dry-run: would remove %s\n", path)
		fmt.Printf("dry-run: would run: systemctl --user daemon-reload\n")
		return nil
	}
	if err := m.run(ctx, "systemctl", "--user", "disable", systemdUnit); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("systemd: remove unit file: %w", err)
	}
	return m.run(ctx, "systemctl", "--user", "daemon-reload")
}

// Start starts the systemd user service.
func (m *SystemdManager) Start(ctx context.Context) error {
	if m.dryRun {
		fmt.Printf("dry-run: would run: systemctl --user start %s\n", systemdUnit)
		return nil
	}
	return m.run(ctx, "systemctl", "--user", "start", systemdUnit)
}

// Stop stops the systemd user service.
func (m *SystemdManager) Stop(ctx context.Context) error {
	if m.dryRun {
		fmt.Printf("dry-run: would run: systemctl --user stop %s\n", systemdUnit)
		return nil
	}
	return m.run(ctx, "systemctl", "--user", "stop", systemdUnit)
}

// Restart restarts the systemd user service.
func (m *SystemdManager) Restart(ctx context.Context) error {
	if m.dryRun {
		fmt.Printf("dry-run: would run: systemctl --user restart %s\n", systemdUnit)
		return nil
	}
	return m.run(ctx, "systemctl", "--user", "restart", systemdUnit)
}

// Status queries systemd for the service state and main PID.
func (m *SystemdManager) Status(ctx context.Context) (ServiceStatus, error) {
	if m.dryRun {
		return ServiceStatus{Message: "dry-run: status not queried"}, nil
	}
	activeOut, _ := exec.CommandContext(ctx, "systemctl", "--user", "is-active", systemdUnit).Output()
	active := strings.TrimSpace(string(activeOut)) == "active"

	pidOut, err := exec.CommandContext(ctx, "systemctl", "--user", "show", systemdUnit, "--property=MainPID").Output()
	if err != nil {
		return ServiceStatus{Running: active, Message: strings.TrimSpace(string(activeOut))}, nil
	}
	st := ServiceStatus{Running: active, Message: strings.TrimSpace(string(activeOut))}
	for line := range strings.SplitSeq(string(pidOut), "\n") {
		if after, ok := strings.CutPrefix(line, "MainPID="); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(after)); err == nil && pid > 0 {
				st.PID = pid
			}
			break
		}
	}
	return st, nil
}

func (m *SystemdManager) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemd: %s %v: %w\n%s", name, args, err, out)
	}
	return nil
}
