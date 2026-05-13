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
	return NewForGOOS(runtime.GOOS, binaryPath, veskaHome)
}

// NewForGOOS returns the Manager for the given GOOS value.
// It is exported so tests can exercise specific platforms.
func NewForGOOS(goos, binaryPath, veskaHome string) (Manager, error) {
	switch goos {
	case "darwin":
		return NewLaunchdManager(binaryPath, veskaHome, false), nil
	case "linux":
		return NewSystemdManager(binaryPath, veskaHome, false), nil
	default:
		return nil, errUnsupportedPlatform
	}
}
