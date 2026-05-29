package doctor

import (
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// ConfigReport holds the result of inspecting the veska configuration.
type ConfigReport struct {
	VeskaHome    string             `json:"veska_home"`
	DBPath       string             `json:"db_path"`
	DBExists     bool               `json:"db_exists"`
	VeskaHomeSet bool               `json:"veska_home_set"`
	Cache        config.CacheConfig `json:"cache"`
	CacheError   string             `json:"cache_error,omitempty"`
}

// CheckConfig stats veska.db inside veskaHome, checks whether the
// VESKA_HOME environment variable is explicitly set, and loads the
// ephemeral-cache eviction knobs. A malformed VESKA_CACHE_* env var
// is surfaced via CacheError so `veska doctor config` shows the user
// what to fix; the report itself never returns a non-nil error.
func CheckConfig(veskaHome string) (ConfigReport, error) {
	dbPath := filepath.Join(veskaHome, "veska.db")
	_, err := os.Stat(dbPath)
	dbExists := err == nil

	rep := ConfigReport{
		VeskaHome:    veskaHome,
		DBPath:       dbPath,
		DBExists:     dbExists,
		VeskaHomeSet: os.Getenv("VESKA_HOME") != "",
	}
	cacheCfg, cacheErr := config.LoadCacheConfig()
	rep.Cache = cacheCfg
	if cacheErr != nil {
		rep.CacheError = cacheErr.Error()
	}
	return rep, nil
}
