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
