// Package service provides platform-specific OS service managers for the
// veska daemon. Use New to get the correct implementation for the current OS.
package service

import (
	"context"
	"errors"
	"runtime"
)

// ServiceStatus describes the current state of the veska daemon service.
type ServiceStatus struct {
	Running bool
	PID     int
	Message string
}

// Manager is the platform-agnostic interface for controlling the veska daemon
// as an OS service (systemd --user on Linux, launchd on macOS).
type Manager interface {
	Install(ctx context.Context) error
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Status(ctx context.Context) (ServiceStatus, error)
}

// errUnsupportedPlatform is returned when the current OS has no service manager.
var errUnsupportedPlatform = errors.New("unsupported platform: no service manager available")

// New returns the Manager appropriate for the current OS.
// binaryPath is the absolute path to the veska-daemon binary.
// veskaHome is the Veska data root (typically ~/.veska).
func New(binaryPath, veskaHome string) (Manager, error) {
	return newForGOOS(runtime.GOOS, binaryPath, veskaHome, false)
}

// NewDryRun mirrors New but returns a Manager whose mutating operations
// print what they would do instead of executing them. Used by the
// `veska service * --dry-run` subcommands so users see the concrete file
// paths and supervisor commands that would run.
func NewDryRun(binaryPath, veskaHome string) (Manager, error) {
	return newForGOOS(runtime.GOOS, binaryPath, veskaHome, true)
}

// NewForGOOS returns the Manager for the given GOOS value.
// It is exported so tests can exercise specific platforms.
func NewForGOOS(goos, binaryPath, veskaHome string) (Manager, error) {
	return newForGOOS(goos, binaryPath, veskaHome, false)
}

// newForGOOS is the shared internal constructor that both New and
// NewDryRun delegate to. Keeps the platform switch in one place.
func newForGOOS(goos, binaryPath, veskaHome string, dryRun bool) (Manager, error) {
	switch goos {
	case "darwin":
		return NewLaunchdManager(binaryPath, veskaHome, dryRun), nil
	case "linux":
		return NewSystemdManager(binaryPath, veskaHome, dryRun), nil
	default:
		return nil, errUnsupportedPlatform
	}
}
