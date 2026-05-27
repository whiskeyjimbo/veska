package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Default values for the cache-tier eviction knobs. Captured here so
// tests and the doctor surface refer to the same constants.
const (
	DefaultCacheDeclinedTTL   = 12 * time.Hour
	DefaultCacheMaxBytes      = int64(5 * 1024 * 1024 * 1024) // 5 GiB
	DefaultCacheMaxEphemerals = 50
)

// CacheConfig captures the three eviction knobs governing the ephemeral
// repo cache (solov2-kxo5.8). The actual sweeper is a follow-up bead;
// kxo5 only plumbs the knobs so retrofitting the sweeper later does not
// require a second migration or another pass over the query code.
//
// Source fields record whether each value came from the corresponding
// VESKA_CACHE_* env var ("env") or the compiled-in default ("default")
// so `veska doctor config` can show the user where each value came from.
type CacheConfig struct {
	DeclinedTTL   time.Duration `json:"declined_ttl"`
	MaxBytes      int64         `json:"max_bytes"`
	MaxEphemerals int           `json:"max_ephemerals"`

	DeclinedTTLSource   string `json:"declined_ttl_source"`
	MaxBytesSource      string `json:"max_bytes_source"`
	MaxEphemeralsSource string `json:"max_ephemerals_source"`
}

// LoadCacheConfig reads the three eviction knobs from the environment,
// falling back to the documented defaults. A malformed value returns an
// error naming the env var so the user can fix it without guessing.
func LoadCacheConfig() (CacheConfig, error) {
	cfg := CacheConfig{
		DeclinedTTL:         DefaultCacheDeclinedTTL,
		MaxBytes:            DefaultCacheMaxBytes,
		MaxEphemerals:       DefaultCacheMaxEphemerals,
		DeclinedTTLSource:   "default",
		MaxBytesSource:      "default",
		MaxEphemeralsSource: "default",
	}

	if v := os.Getenv("VESKA_CACHE_DECLINED_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("VESKA_CACHE_DECLINED_TTL=%q: %w", v, err)
		}
		if d <= 0 {
			return cfg, fmt.Errorf("VESKA_CACHE_DECLINED_TTL=%q: must be positive", v)
		}
		cfg.DeclinedTTL = d
		cfg.DeclinedTTLSource = "env"
	}

	if v := os.Getenv("VESKA_CACHE_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("VESKA_CACHE_MAX_BYTES=%q: %w", v, err)
		}
		if n < 0 {
			return cfg, fmt.Errorf("VESKA_CACHE_MAX_BYTES=%q: must be non-negative", v)
		}
		cfg.MaxBytes = n
		cfg.MaxBytesSource = "env"
	}

	if v := os.Getenv("VESKA_CACHE_MAX_EPHEMERALS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("VESKA_CACHE_MAX_EPHEMERALS=%q: %w", v, err)
		}
		if n < 0 {
			return cfg, fmt.Errorf("VESKA_CACHE_MAX_EPHEMERALS=%q: must be non-negative", v)
		}
		cfg.MaxEphemerals = n
		cfg.MaxEphemeralsSource = "env"
	}

	return cfg, nil
}
