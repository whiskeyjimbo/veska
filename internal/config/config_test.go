package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Embedder.RatePerSec != 10 {
		t.Errorf("embedder.rate_per_sec: got %v want 10", c.Embedder.RatePerSec)
	}
	if c.PostPromotionQueue.PollInterval != "250ms" {
		t.Errorf("post_promotion_queue.poll_interval: got %q want 250ms", c.PostPromotionQueue.PollInterval)
	}
	if c.Budget.DefaultTokens != 8192 {
		t.Errorf("budget.default_tokens: got %v want 8192", c.Budget.DefaultTokens)
	}
	if c.Budget.CeilingTokens != 24000 {
		t.Errorf("budget.ceiling_tokens: got %v want 24000", c.Budget.CeilingTokens)
	}
	if c.Metrics.Enabled {
		t.Error("metrics should be disabled by default")
	}
	if c.Tracing.Enabled {
		t.Error("tracing should be disabled by default")
	}
	if c.Embedder.Endpoint != "http://localhost:11434" {
		t.Errorf("embedder.endpoint: got %q", c.Embedder.Endpoint)
	}
}

func TestLoadNoFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	clearOverrideEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := DefaultConfig()
	if c.Embedder.RatePerSec != def.Embedder.RatePerSec {
		t.Errorf("missing file should yield defaults: got %v", c.Embedder.RatePerSec)
	}
}

func TestLoadDecodesFileOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	clearOverrideEnv(t)

	toml := `
[embedder]
rate_per_sec = 25

[post_promotion_queue]
poll_interval = "500ms"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Embedder.RatePerSec != 25 {
		t.Errorf("file override embedder.rate_per_sec: got %v want 25", c.Embedder.RatePerSec)
	}
	if c.PostPromotionQueue.PollInterval != "500ms" {
		t.Errorf("file override poll_interval: got %q want 500ms", c.PostPromotionQueue.PollInterval)
	}
	// Untouched key keeps its default.
	if c.Budget.DefaultTokens != 8192 {
		t.Errorf("untouched key should keep default: got %v", c.Budget.DefaultTokens)
	}
}

func TestLoadEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	clearOverrideEnv(t)

	toml := `
[embedder]
endpoint = "http://from-file:11434"
model = "from-file-model"

[storage]
vector_backend = "sqlite-vec"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("VESKA_OLLAMA_URL", "http://from-env:11434")
	t.Setenv("VESKA_EMBED_MODEL", "from-env-model")
	t.Setenv("VESKA_VECTOR_BACKEND", "usearch")
	t.Setenv("VESKA_DEBUG", "1")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Embedder.Endpoint != "http://from-env:11434" {
		t.Errorf("env should override file endpoint: got %q", c.Embedder.Endpoint)
	}
	if c.Embedder.Model != "from-env-model" {
		t.Errorf("env should override file model: got %q", c.Embedder.Model)
	}
	if c.Storage.VectorBackend != "usearch" {
		t.Errorf("env should override file vector_backend: got %q", c.Storage.VectorBackend)
	}
	if c.Logging.Level != "debug" {
		t.Errorf("VESKA_DEBUG should set logging level to debug: got %q", c.Logging.Level)
	}
}

func TestValidateRejectsTracingWithoutEndpoint(t *testing.T) {
	c := DefaultConfig()
	c.Tracing.Enabled = true
	c.Tracing.OTLPEndpoint = ""
	if err := c.Validate(); err == nil {
		t.Error("tracing enabled without OTLP endpoint should be a config error")
	}

	c.Tracing.OTLPEndpoint = "http://localhost:4318"
	if err := c.Validate(); err != nil {
		t.Errorf("tracing enabled with endpoint should validate: %v", err)
	}
}

func TestValidateRejectsEndpointWithoutTracing(t *testing.T) {
	c := DefaultConfig()
	c.Tracing.Enabled = false
	c.Tracing.OTLPEndpoint = "localhost:4317"
	if err := c.Validate(); err == nil {
		t.Error("otlp_endpoint set with tracing disabled should be a config error")
	}
}

func TestEnvOverridesOTLPEndpoint(t *testing.T) {
	clearOverrideEnv(t)
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	t.Setenv("VESKA_OTLP_ENDPOINT", "localhost:4317")

	c := DefaultConfig()
	applyEnvOverrides(&c)
	if c.Tracing.OTLPEndpoint != "localhost:4317" {
		t.Errorf("VESKA_OTLP_ENDPOINT should override otlp_endpoint: got %q", c.Tracing.OTLPEndpoint)
	}
}

func TestVulnSourceDefaultsOff(t *testing.T) {
	c := DefaultConfig()
	if c.VulnSource.Provider != "" {
		t.Errorf("vuln_source.provider should default to empty (off), got %q", c.VulnSource.Provider)
	}
}

func TestLoadDecodesVulnSourceSection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	clearOverrideEnv(t)

	toml := `
[vuln_source]
provider = "osv"
refresh_interval = "6h"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.VulnSource.Provider != "osv" {
		t.Errorf("vuln_source.provider: got %q want osv", c.VulnSource.Provider)
	}
	if c.VulnSource.RefreshInterval != "6h" {
		t.Errorf("vuln_source.refresh_interval: got %q want 6h", c.VulnSource.RefreshInterval)
	}
}

func TestPromotionDefaultsNoDisabledChecks(t *testing.T) {
	c := DefaultConfig()
	if len(c.Promotion.DisabledChecks) != 0 {
		t.Errorf("promotion.disabled_checks should default to empty, got %v", c.Promotion.DisabledChecks)
	}
}

func TestLoadDecodesPromotionDisabledChecks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	clearOverrideEnv(t)

	toml := `
[promotion]
disabled_checks = ["secrets-scan"]
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Promotion.CheckDisabled("secrets-scan") {
		t.Errorf("promotion.CheckDisabled(secrets-scan): got false, want true")
	}
	if c.Promotion.CheckDisabled("dead-code") {
		t.Errorf("promotion.CheckDisabled(dead-code): got true, want false")
	}
}

func clearOverrideEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"VESKA_OLLAMA_URL", "VESKA_EMBED_MODEL", "VESKA_VECTOR_BACKEND", "VESKA_DEBUG"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}
