// Package config provides configuration constants and path helpers.
package config

import (
	"os"
	"path/filepath"
)

// engramHome returns the root Engram data directory.
// It uses $ENGRAM_HOME when set; otherwise ~/.engram.
// Falls back to ".engram" when the home directory cannot be determined.
func engramHome() string {
	if dir := os.Getenv("ENGRAM_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".engram"
	}
	return filepath.Join(home, ".engram")
}

// DefaultVectorDir returns the default directory for persisted vector index files.
// It resolves to ~/.engram using os.UserHomeDir.
// If the home directory cannot be determined, it falls back to ".engram" relative
// to the working directory.
func DefaultVectorDir() string {
	return engramHome()
}

// DaemonSockPath returns the Unix-domain socket path used by the engram daemon.
// It resolves to $ENGRAM_HOME/daemon.sock, where ENGRAM_HOME defaults to ~/.engram.
func DaemonSockPath() string {
	return filepath.Join(engramHome(), "daemon.sock")
}

// MCPSockPath returns the path of the MCP Unix socket (~/.engram/mcp.sock).
// Respects ENGRAM_HOME env var.
func MCPSockPath() string {
	return filepath.Join(engramHome(), "mcp.sock")
}
