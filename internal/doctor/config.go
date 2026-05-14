package doctor

import (
	"os"
	"path/filepath"
)

// ConfigReport holds the result of inspecting the veska configuration.
type ConfigReport struct {
	VeskaHome    string `json:"veska_home"`
	DBPath       string `json:"db_path"`
	DBExists     bool   `json:"db_exists"`
	VeskaHomeSet bool   `json:"veska_home_set"`
}

// CheckConfig stats veska.db inside veskaHome and checks whether the
// VESKA_HOME environment variable is explicitly set.  It never returns a
// non-nil error.
func CheckConfig(veskaHome string) (ConfigReport, error) {
	dbPath := filepath.Join(veskaHome, "veska.db")
	_, err := os.Stat(dbPath)
	dbExists := err == nil

	return ConfigReport{
		VeskaHome:    veskaHome,
		DBPath:       dbPath,
		DBExists:     dbExists,
		VeskaHomeSet: os.Getenv("VESKA_HOME") != "",
	}, nil
}
