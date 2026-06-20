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

const launchdLabel = "com.veska.daemon"

// plistTemplate is the embedded launchd plist template content.
const plistTemplateText = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.veska.daemon</string>

	<key>ProgramArguments</key>
	<array>
		<string>{{.BinaryPath}}</string>
	</array>

	<key>EnvironmentVariables</key>
	<dict>
		<key>VESKA_HOME</key>
		<string>{{.VeskaHome}}</string>
	</dict>

	<key>RunAtLoad</key>
	<true/>

	<key>KeepAlive</key>
	<true/>

	<key>StandardOutPath</key>
	<string>{{.VeskaHome}}/daemon.log</string>

	<key>StandardErrorPath</key>
	<string>{{.VeskaHome}}/daemon.log</string>
</dict>
</plist>
`

// LaunchdManager manages the veska daemon via launchd on macOS.
type LaunchdManager struct {
	binaryPath string
	veskaHome  string
	dryRun     bool
}

// When dryRun is true, Install/Uninstall/Start/Stop/Restart print what they
// would do rather than executing any commands.
func NewLaunchdManager(binaryPath, veskaHome string, dryRun bool) *LaunchdManager {
	return &LaunchdManager{
		binaryPath: binaryPath,
		veskaHome:  veskaHome,
		dryRun:     dryRun,
	}
}

// plistPath returns the user-agent plist path.
func (m *LaunchdManager) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("launchd: resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// RenderPlist renders the launchd plist template for the given binary and home.
// Exported for testing.
func RenderPlist(binaryPath, veskaHome string) (string, error) {
	tmpl, err := template.New("plist").Parse(plistTemplateText)
	if err != nil {
		return "", fmt.Errorf("launchd: parse plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		BinaryPath string
		VeskaHome  string
	}{binaryPath, veskaHome}); err != nil {
		return "", fmt.Errorf("launchd: render plist: %w", err)
	}
	return buf.String(), nil
}

// Install writes the plist and loads it via launchctl.
func (m *LaunchdManager) Install(ctx context.Context) error {
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	content, err := RenderPlist(m.binaryPath, m.veskaHome)
	if err != nil {
		return err
	}
	if m.dryRun {
		fmt.Printf("dry-run: would write plist to %s\n", path)
		fmt.Printf("dry-run: would run: launchctl load -w %s\n", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("launchd: create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("launchd: write plist: %w", err)
	}
	return m.run(ctx, "launchctl", "load", "-w", path)
}

// Uninstall unloads the service and removes the plist.
func (m *LaunchdManager) Uninstall(ctx context.Context) error {
	path, err := m.plistPath()
	if err != nil {
		return err
	}
	if m.dryRun {
		fmt.Printf("dry-run: would run: launchctl unload -w %s\n", path)
		fmt.Printf("dry-run: would remove %s\n", path)
		return nil
	}
	if err := m.run(ctx, "launchctl", "unload", "-w", path); err != nil {
		return err
	}
	return os.Remove(path)
}

// Start starts the launchd service.
func (m *LaunchdManager) Start(ctx context.Context) error {
	if m.dryRun {
		fmt.Printf("dry-run: would run: launchctl start %s\n", launchdLabel)
		return nil
	}
	return m.run(ctx, "launchctl", "start", launchdLabel)
}

// Stop stops the launchd service.
func (m *LaunchdManager) Stop(ctx context.Context) error {
	if m.dryRun {
		fmt.Printf("dry-run: would run: launchctl stop %s\n", launchdLabel)
		return nil
	}
	return m.run(ctx, "launchctl", "stop", launchdLabel)
}

// Restart restarts the launchd service by stopping then starting.
func (m *LaunchdManager) Restart(ctx context.Context) error {
	if m.dryRun {
		fmt.Printf("dry-run: would stop then start %s\n", launchdLabel)
		return nil
	}
	// launchctl has no atomic restart; stop + start is idiomatic.
	_ = m.run(ctx, "launchctl", "stop", launchdLabel) // ignore error if not running
	return m.run(ctx, "launchctl", "start", launchdLabel)
}

// Status queries launchctl and returns the service state.
func (m *LaunchdManager) Status(ctx context.Context) (ServiceStatus, error) {
	if m.dryRun {
		return ServiceStatus{Message: "dry-run: status not queried"}, nil
	}
	out, err := exec.CommandContext(ctx, "launchctl", "list", launchdLabel).Output()
	if err != nil {
		return ServiceStatus{Running: false, Message: "not running"}, nil
	}
	st := ServiceStatus{Running: true, Message: strings.TrimSpace(string(out))}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 {
			pid, err := strconv.Atoi(fields[0])
			if err == nil && pid > 0 {
				st.PID = pid
				break
			}
		}
	}
	return st, nil
}

func (m *LaunchdManager) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchd: %s %v: %w\n%s", name, args, err, out)
	}
	return nil
}
