// Package config provides configuration constants and path helpers.
package config

import (
	"os"
	"path/filepath"
)

// veskaHome returns the root Veska data directory.
// It uses $VESKA_HOME when set; otherwise ~/.veska.
// Falls back to ".veska" when the home directory cannot be determined.
func veskaHome() string {
	if dir := os.Getenv("VESKA_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".veska"
	}
	return filepath.Join(home, ".veska")
}

// DefaultVectorDir returns the default directory for persisted vector index files.
// It resolves to ~/.veska using os.UserHomeDir.
// If the home directory cannot be determined, it falls back to ".veska" relative
// to the working directory.
func DefaultVectorDir() string {
	return veskaHome()
}

// DaemonSockPath returns the Unix-domain socket path used by the veska daemon.
// It resolves to $VESKA_HOME/daemon.sock, where VESKA_HOME defaults to ~/.veska.
func DaemonSockPath() string {
	return filepath.Join(veskaHome(), "daemon.sock")
}

// MCPSockPath returns the path of the MCP Unix socket (~/.veska/mcp.sock).
// Respects VESKA_HOME env var.
func MCPSockPath() string {
	return filepath.Join(veskaHome(), "mcp.sock")
}
