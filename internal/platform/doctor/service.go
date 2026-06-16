package doctor

import (
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// ServiceReport holds the results of the service health probe.
type ServiceReport struct {
	DaemonRunning       bool          `json:"daemon_running"`
	BrokenMarkerPresent bool          `json:"broken_marker_present"`
	BrokenMarkerPath    string        `json:"broken_marker_path,omitempty"`
	Status              health.Status `json:"status"`
}

// CheckService probes the daemon socket and broken marker for veskaHome.
// The daemon socket is expected at <veskaHome>/cli.sock.
// The broken marker is expected at <veskaHome>/broken.
// Status rules:
//
//	"broken" — broken marker file is present (regardless of daemon state)
//	"degraded" — no broken marker but daemon socket is unreachable
//	"healthy" — daemon running and no broken marker
func CheckService(veskaHome string) (ServiceReport, error) {
	markerPath := filepath.Join(veskaHome, "broken")
	sockPath := filepath.Join(veskaHome, "cli.sock")

	// 1. Check broken marker.
	_, err := os.Stat(markerPath)
	brokenMarkerPresent := err == nil

	// 2. Dial daemon socket with 200ms timeout.
	daemonRunning := false
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		daemonRunning = true
	}

	// 3. Compute status.
	status := health.StatusHealthy
	switch {
	case brokenMarkerPresent:
		status = health.StatusBroken
	case !daemonRunning:
		status = health.StatusDegraded
	}

	report := ServiceReport{
		DaemonRunning:       daemonRunning,
		BrokenMarkerPresent: brokenMarkerPresent,
		Status:              status,
	}
	if brokenMarkerPresent {
		report.BrokenMarkerPath = markerPath
	}
	return report, nil
}
