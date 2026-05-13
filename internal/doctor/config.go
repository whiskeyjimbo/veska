package doctor

import (
	"os"
	"path/filepath"
)

// ConfigReport holds the result of inspecting the engram configuration.
type ConfigReport struct {
	EngramHome    string `json:"engram_home"`
	DBPath        string `json:"db_path"`
	DBExists      bool   `json:"db_exists"`
	EngramHomeSet bool   `json:"engram_home_set"`
}

// CheckConfig stats engram.db inside engramHome and checks whether the
// ENGRAM_HOME environment variable is explicitly set.  It never returns a
// non-nil error.
func CheckConfig(engramHome string) (ConfigReport, error) {
	dbPath := filepath.Join(engramHome, "engram.db")
	_, err := os.Stat(dbPath)
	dbExists := err == nil

	return ConfigReport{
		EngramHome:    engramHome,
		DBPath:        dbPath,
		DBExists:      dbExists,
		EngramHomeSet: os.Getenv("ENGRAM_HOME") != "",
	}, nil
}
