// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

func TestLoadCacheConfig_Defaults(t *testing.T) {
	t.Setenv("VESKA_CACHE_DECLINED_TTL", "")
	t.Setenv("VESKA_CACHE_MAX_BYTES", "")
	t.Setenv("VESKA_CACHE_MAX_EPHEMERALS", "")

	cfg, err := config.LoadCacheConfig()
	if err != nil {
		t.Fatalf("LoadCacheConfig: %v", err)
	}
	if cfg.DeclinedTTL != config.DefaultCacheDeclinedTTL {
		t.Errorf("DeclinedTTL = %v, want default %v", cfg.DeclinedTTL, config.DefaultCacheDeclinedTTL)
	}
	if cfg.MaxBytes != config.DefaultCacheMaxBytes {
		t.Errorf("MaxBytes = %d, want default %d", cfg.MaxBytes, config.DefaultCacheMaxBytes)
	}
	if cfg.MaxEphemerals != config.DefaultCacheMaxEphemerals {
		t.Errorf("MaxEphemerals = %d, want default %d", cfg.MaxEphemerals, config.DefaultCacheMaxEphemerals)
	}
	for _, src := range []string{cfg.DeclinedTTLSource, cfg.MaxBytesSource, cfg.MaxEphemeralsSource} {
		if src != "default" {
			t.Errorf("source = %q, want default", src)
		}
	}
}

func TestLoadCacheConfig_EnvOverrides(t *testing.T) {
	t.Setenv("VESKA_CACHE_DECLINED_TTL", "30m")
	t.Setenv("VESKA_CACHE_MAX_BYTES", "1073741824") // 1 GiB
	t.Setenv("VESKA_CACHE_MAX_EPHEMERALS", "7")

	cfg, err := config.LoadCacheConfig()
	if err != nil {
		t.Fatalf("LoadCacheConfig: %v", err)
	}
	if cfg.DeclinedTTL != 30*time.Minute {
		t.Errorf("DeclinedTTL = %v, want 30m", cfg.DeclinedTTL)
	}
	if cfg.MaxBytes != 1<<30 {
		t.Errorf("MaxBytes = %d, want 1 GiB", cfg.MaxBytes)
	}
	if cfg.MaxEphemerals != 7 {
		t.Errorf("MaxEphemerals = %d, want 7", cfg.MaxEphemerals)
	}
	for _, src := range []string{cfg.DeclinedTTLSource, cfg.MaxBytesSource, cfg.MaxEphemeralsSource} {
		if src != "env" {
			t.Errorf("source = %q, want env", src)
		}
	}
}

func TestLoadCacheConfig_InvalidValuesNameTheKnob(t *testing.T) {
	cases := []struct {
		env, val, wantSubstr string
	}{
		{"VESKA_CACHE_DECLINED_TTL", "abc", "VESKA_CACHE_DECLINED_TTL"},
		{"VESKA_CACHE_DECLINED_TTL", "-5m", "VESKA_CACHE_DECLINED_TTL"},
		{"VESKA_CACHE_MAX_BYTES", "huge", "VESKA_CACHE_MAX_BYTES"},
		{"VESKA_CACHE_MAX_BYTES", "-1", "VESKA_CACHE_MAX_BYTES"},
		{"VESKA_CACHE_MAX_EPHEMERALS", "lots", "VESKA_CACHE_MAX_EPHEMERALS"},
		{"VESKA_CACHE_MAX_EPHEMERALS", "-3", "VESKA_CACHE_MAX_EPHEMERALS"},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.val, func(t *testing.T) {
			t.Setenv("VESKA_CACHE_DECLINED_TTL", "")
			t.Setenv("VESKA_CACHE_MAX_BYTES", "")
			t.Setenv("VESKA_CACHE_MAX_EPHEMERALS", "")
			t.Setenv(tc.env, tc.val)
			_, err := config.LoadCacheConfig()
			if err == nil {
				t.Fatalf("expected error for %s=%q", tc.env, tc.val)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q missing knob name %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}
