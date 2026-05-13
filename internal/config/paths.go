// Package config provides configuration constants and path helpers.
package config

import (
	"os"
	"path/filepath"
)

// DefaultVectorDir returns the default directory for persisted vector index files.
// It resolves to ~/.engram using os.UserHomeDir.
// If the home directory cannot be determined, it falls back to ".engram" relative
// to the working directory.
func DefaultVectorDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".engram"
	}
	return filepath.Join(home, ".engram")
}

// DaemonSockPath returns the Unix-domain socket path used by the engram daemon.
// It resolves to $ENGRAM_HOME/daemon.sock, where ENGRAM_HOME defaults to ~/.engram.
func DaemonSockPath() string {
	if dir := os.Getenv("ENGRAM_HOME"); dir != "" {
		return filepath.Join(dir, "daemon.sock")
	}
	return filepath.Join(DefaultVectorDir(), "daemon.sock")
}
