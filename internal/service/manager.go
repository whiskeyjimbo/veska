// Package service provides platform-specific OS service managers for the
// engram daemon. Use New to get the correct implementation for the current OS.
package service

import (
	"context"
	"errors"
	"runtime"
)

// ServiceStatus describes the current state of the engram daemon service.
type ServiceStatus struct {
	Running bool
	PID     int
	Message string
}

// Manager is the platform-agnostic interface for controlling the engram daemon
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
// binaryPath is the absolute path to the engram-daemon binary.
// engramHome is the Engram data root (typically ~/.engram).
func New(binaryPath, engramHome string) (Manager, error) {
	return NewForGOOS(runtime.GOOS, binaryPath, engramHome)
}

// NewForGOOS returns the Manager for the given GOOS value.
// It is exported so tests can exercise specific platforms.
func NewForGOOS(goos, binaryPath, engramHome string) (Manager, error) {
	switch goos {
	case "darwin":
		return NewLaunchdManager(binaryPath, engramHome, false), nil
	case "linux":
		return NewSystemdManager(binaryPath, engramHome, false), nil
	default:
		return nil, errUnsupportedPlatform
	}
}
