package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the resolved daemon configuration. It mirrors the documented
// surface in docs/operations/CONFIG-SURFACE.md. Sections that map to real
// current behaviour are loaded and consumed today; M5 sections (budget,
// llm_generator, review) are decoded and validated but not all consumed yet.
type Config struct {
	Daemon             DaemonConfig             `toml:"daemon"`
	Logging            LoggingConfig            `toml:"logging"`
	Metrics            MetricsConfig            `toml:"metrics"`
	Tracing            TracingConfig            `toml:"tracing"`
	Storage            StorageConfig            `toml:"storage"`
	Watcher            WatcherConfig            `toml:"watcher"`
	Embedder           EmbedderConfig           `toml:"embedder"`
	PostPromotionQueue PostPromotionQueueConfig `toml:"post_promotion_queue"`
	Budget             BudgetConfig             `toml:"budget"`
	LLMGenerator       LLMGeneratorConfig       `toml:"llm_generator"`
	Review             ReviewConfig             `toml:"review"`
	Backup             BackupConfig             `toml:"backup"`
	VulnSource         VulnSourceConfig         `toml:"vuln_source"`
	Promotion          PromotionConfig          `toml:"promotion"`
	Wiki               WikiConfig               `toml:"wiki"`
	Autolink           AutolinkConfig           `toml:"autolink"`
	Blast              BlastConfig              `toml:"blast"`
}

// AutolinkConfig tunes the auto-link candidate computation (solov2-l8su). The
// defaults mirror the autolink package constants (DefaultThreshold,
// DefaultTopK) calibrated against the gate-3 nomic-embed-text fixture. Most
// users should never touch these; a different embedder or repository layout is
// the reason to. The score-space caveat applies: Threshold is a lower bound on
// the higher-is-closer similarity 1/(1+L2dist), only meaningful on
// L2-normalised embeddings.
type AutolinkConfig struct {
	// Threshold is the minimum similarity for a candidate edge to be emitted.
	// Range [0, 1]; 0 admits every non-self neighbour.
	Threshold float64 `toml:"threshold"`
	// TopK is the per-source candidate cap. Must be > 0.
	TopK int `toml:"top_k"`
}

// BlastConfig tunes the blast-radius BFS heuristics (solov2-l8su).
type BlastConfig struct {
	// HubDegreeThreshold gates BFS expansion through high-degree "registry"
	// nodes (cobra rootCmd, http muxes). Nodes whose neighbour count exceeds
	// this are reported but not expanded through. A negative value disables
	// the gate entirely (legacy expand-through-everything behaviour); 0 is
	// rejected by Validate so the disable intent is always explicit.
	HubDegreeThreshold int `toml:"hub_degree_threshold"`
}

// WikiConfig controls the developer-wiki Markdown pages. The README's product
// contract is that veska "writes no files to disk" by default; the
// hot_zone / entry_points pages are an opt-in convenience for users who want
// them committed alongside their source. They're computed from the graph
// either way, and the MCP tools eng_get_hot_zone / eng_get_entry_points
// surface the same data on demand regardless of this flag .
type WikiConfig struct {
	// WritePages, when true, makes the wiki handler write docs/veska/*.md
	// into the user's repo working tree on every promotion. Default false —
	// no files are written, matching the README's "no files written to disk"
	// claim. Flip on per-repo via a docs/veska/.veska-wiki sentinel file once
	// that flow lands; for now this is a global daemon switch.
	WritePages bool `toml:"write_pages"`
}

// PromotionConfig tunes the synchronous promotion-pipeline checks (M7). Every
// structural check ships on by default; listing a check's Name() in
// DisabledChecks suppresses its registration in the daemon composition root.
type PromotionConfig struct {
	// DisabledChecks names structural checks to skip. Each entry matches a
	// check's Name() (e.g. "secrets-scan"). An empty list keeps every check on.
	DisabledChecks []string `toml:"disabled_checks"`
}

// CheckDisabled reports whether the named check appears in DisabledChecks.
func (c PromotionConfig) CheckDisabled(name string) bool {
	return slices.Contains(c.DisabledChecks, name)
}

// DaemonConfig holds socket paths and the graceful-stop window.
type DaemonConfig struct {
	CLISocketPath string `toml:"cli_socket_path"`
	MCPSocketPath string `toml:"mcp_socket_path"`
	PIDFile       string `toml:"pid_file"`
	ShutdownGrace string `toml:"shutdown_grace"`
}

// LoggingConfig controls log format, level and rotation.
type LoggingConfig struct {
	Format        string `toml:"format"`
	Level         string `toml:"level"`
	File          string `toml:"file"`
	RotateAtBytes int64  `toml:"rotate_at_bytes"`
	KeepRotations int    `toml:"keep_rotations"`
}

// MetricsConfig is the opt-in Prometheus listener.
type MetricsConfig struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
}

// TracingConfig is the opt-in OTLP tracing exporter.
type TracingConfig struct {
	Enabled      bool    `toml:"enabled"`
	OTLPEndpoint string  `toml:"otlp_endpoint"`
	SampleRatio  float64 `toml:"sample_ratio"`
}

// StorageConfig holds the SQLite + vector storage knobs.
type StorageConfig struct {
	DBPath              string `toml:"db_path"`
	JournalMode         string `toml:"journal_mode"`
	Synchronous         string `toml:"synchronous"`
	WALAutocheckpoint   int    `toml:"wal_autocheckpoint"`
	IdleCheckpointAfter string `toml:"idle_checkpoint_after"`
	VectorBackend       string `toml:"vector_backend"`
}

// WatcherConfig holds fsnotify debounce + admission ceilings.
type WatcherConfig struct {
	Debounce             string `toml:"debounce"`
	PollFallbackInterval string `toml:"poll_fallback_interval"`
	WakeThreshold        string `toml:"wake_threshold"`
	WakeTick             string `toml:"wake_tick"`
	WakeConcurrency      int    `toml:"wake_concurrency"`
	MaxPathsPerRepo      int    `toml:"max_paths_per_repo"`
	MaxPathsTotal        int    `toml:"max_paths_total"`
}

// EmbedderConfig selects the embedding provider and its rate limits.
type EmbedderConfig struct {
	Provider   string  `toml:"provider"`
	Endpoint   string  `toml:"endpoint"`
	Model      string  `toml:"model"`
	Dim        int     `toml:"dim"`
	RatePerSec float64 `toml:"rate_per_sec"`
	BatchSize  int     `toml:"batch_size"`
}

// PostPromotionQueueConfig tunes the background work queue poller.
type PostPromotionQueueConfig struct {
	PollInterval  string `toml:"poll_interval"`
	HighWater     int    `toml:"high_water"`
	LowWater      int    `toml:"low_water"`
	DoneRetention string `toml:"done_retention"`
}

// BudgetConfig holds token budgets for the review pipeline (M5).
type BudgetConfig struct {
	DefaultTokens                  int `toml:"default_tokens"`
	CeilingTokens                  int `toml:"ceiling_tokens"`
	RefactorCommitThresholdSymbols int `toml:"refactor_commit_threshold_symbols"`
}

// LLMGeneratorConfig configures the review-pipeline LLM generator (M5).
type LLMGeneratorConfig struct {
	Enabled  bool   `toml:"enabled"`
	Provider string `toml:"provider"`
	Endpoint string `toml:"endpoint"`
	Model    string `toml:"model"`
	Timeout  string `toml:"timeout"`
}

// ReviewConfig holds the review-pipeline token caps (M5).
type ReviewConfig struct {
	Enabled            bool `toml:"enabled"`
	MaxTokensPerCommit int  `toml:"max_tokens_per_commit"`
	MaxTokensPerDay    int  `toml:"max_tokens_per_day"`
}

// BackupConfig holds the retention policy for veska backup prune (SOLO-17 §4.5).
type BackupConfig struct {
	// KeepMinCount is the number of most-recent user-initiated backups always
	// kept regardless of age.
	KeepMinCount int `toml:"keep_min_count"`
	// KeepMaxAge deletes user-initiated backups older than this duration,
	// subject to KeepMinCount. Expressed as a Go duration string (e.g. "30d"
	// is normalised to hours; "720h").
	KeepMaxAge string `toml:"keep_max_age"`
}

// VulnSourceConfig configures the vulnerability advisory source (M7). The
// feature ships off by default: an empty Provider leaves the daemon on the
// NullVulnSource — no refresher goroutine, no vulnscan check. Setting
// Provider = "osv" turns on the OSV.dev-backed adapter.
type VulnSourceConfig struct {
	// Provider selects the VulnSource implementation. "" (default) keeps the
	// feature off; "osv" enables the OSV.dev adapter. Any other value is a
	// fatal startup error.
	Provider string `toml:"provider"`
	// RefreshInterval overrides the advisory-cache refresh cadence as a Go
	// duration string (e.g. "6h"). Empty falls back to the refresher default.
	RefreshInterval string `toml:"refresh_interval"`
}

// DefaultConfig returns the compile-time defaults. These mirror the Go
// constants currently spread across the daemon (embedder.DefaultRatePerSec,
// queue's 250ms poll interval, the chars/4 token budgets, etc.).
func DefaultConfig() Config {
	return Config{
		Daemon: DaemonConfig{
			CLISocketPath: "~/.veska/cli.sock",
			MCPSocketPath: "~/.veska/mcp.sock",
			PIDFile:       "~/.veska/daemon.pid",
			ShutdownGrace: "5s",
		},
		Logging: LoggingConfig{
			Format:        "text",
			Level:         "info",
			File:          "~/.veska/logs/daemon.log",
			RotateAtBytes: 104857600,
			KeepRotations: 5,
		},
		Metrics: MetricsConfig{
			Enabled: false,
			Listen:  "127.0.0.1:9090",
		},
		Tracing: TracingConfig{
			Enabled:      false,
			OTLPEndpoint: "",
			SampleRatio:  1.0,
		},
		Storage: StorageConfig{
			DBPath:              "~/.veska/veska.db",
			JournalMode:         "WAL",
			Synchronous:         "FULL",
			WALAutocheckpoint:   1000,
			IdleCheckpointAfter: "5s",
			VectorBackend:       "memory",
		},
		Watcher: WatcherConfig{
			Debounce:             "200ms",
			PollFallbackInterval: "5s",
			WakeThreshold:        "30s",
			WakeTick:             "5s",
			WakeConcurrency:      0,
			MaxPathsPerRepo:      50000,
			MaxPathsTotal:        200000,
		},
		Embedder: EmbedderConfig{
			Provider:   "ollama",
			Endpoint:   "http://localhost:11434",
			Model:      "nomic-embed-text",
			Dim:        768,
			RatePerSec: 10,
			BatchSize:  32,
		},
		PostPromotionQueue: PostPromotionQueueConfig{
			PollInterval:  "250ms",
			HighWater:     10000,
			LowWater:      8000,
			DoneRetention: "168h",
		},
		Budget: BudgetConfig{
			DefaultTokens:                  8192,
			CeilingTokens:                  24000,
			RefactorCommitThresholdSymbols: 5000,
		},
		LLMGenerator: LLMGeneratorConfig{
			Enabled:  false,
			Provider: "ollama",
			Endpoint: "http://localhost:11434",
			Model:    "llama3.1:8b",
			Timeout:  "60s",
		},
		Review: ReviewConfig{
			Enabled:            false,
			MaxTokensPerCommit: 100000,
			MaxTokensPerDay:    500000,
		},
		Backup: BackupConfig{
			KeepMinCount: 3,
			KeepMaxAge:   "30d",
		},
		Autolink: AutolinkConfig{
			Threshold: 0.60,
			TopK:      5,
		},
		Blast: BlastConfig{
			HubDegreeThreshold: 50,
		},
	}
}

// configPath returns the resolved path of ~/.veska/config.toml, honouring
// VESKA_CONFIG (explicit override) then VESKA_HOME.
func configPath() string {
	if p := os.Getenv("VESKA_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(veskaHome(), "config.toml")
}

// Load resolves the daemon configuration with precedence
// defaults < config.toml < environment variables. A missing config.toml is
// not an error — the compile-time defaults stand.
func Load() (Config, error) {
	cfg := DefaultConfig()

	path := configPath()
	if _, err := os.Stat(path); err == nil {
		if _, derr := toml.DecodeFile(path, &cfg); derr != nil {
			return Config{}, fmt.Errorf("config: decode %s: %w", path, derr)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("config: stat %s: %w", path, err)
	}

	if err := applyEnvOverrides(&cfg); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnvOverrides folds the daemon's environment variables over the resolved
// struct. They are the last (highest-precedence) overlay. Numeric overrides
// (the autolink/blast tuning knobs) return a parse error rather than silently
// ignoring a malformed value — consistent with Load's fail-loud contract.
func applyEnvOverrides(cfg *Config) error {
	if v := os.Getenv("VESKA_OLLAMA_URL"); v != "" {
		cfg.Embedder.Endpoint = v
	}
	if v := os.Getenv("VESKA_EMBED_MODEL"); v != "" {
		cfg.Embedder.Model = v
	}
	if v := os.Getenv("VESKA_VECTOR_BACKEND"); v != "" {
		cfg.Storage.VectorBackend = v
	}
	if v := os.Getenv("VESKA_DEBUG"); v != "" && v != "0" {
		cfg.Logging.Level = "debug"
	}
	if v := os.Getenv("VESKA_OTLP_ENDPOINT"); v != "" {
		cfg.Tracing.OTLPEndpoint = v
	}
	if v := os.Getenv("VESKA_HUB_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: VESKA_HUB_THRESHOLD %q is not an integer: %w", v, err)
		}
		cfg.Blast.HubDegreeThreshold = n
	}
	if v := os.Getenv("VESKA_AUTOLINK_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: VESKA_AUTOLINK_THRESHOLD %q is not a number: %w", v, err)
		}
		cfg.Autolink.Threshold = f
	}
	if v := os.Getenv("VESKA_AUTOLINK_TOPK"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: VESKA_AUTOLINK_TOPK %q is not an integer: %w", v, err)
		}
		cfg.Autolink.TopK = n
	}
	return nil
}

// Validate enforces cross-field invariants. It covers the documented tracing
// both-or-neither rule: tracing.enabled requires an OTLP endpoint, and an
// endpoint without tracing.enabled is a misconfiguration — both are startup
// errors so the operator's intent is never silently ignored.
func (c Config) Validate() error {
	if c.Tracing.Enabled && c.Tracing.OTLPEndpoint == "" {
		return fmt.Errorf("config: tracing enabled but no otlp_endpoint set (set tracing.otlp_endpoint or VESKA_OTLP_ENDPOINT)")
	}
	if !c.Tracing.Enabled && c.Tracing.OTLPEndpoint != "" {
		return fmt.Errorf("config: otlp_endpoint set but tracing is disabled (set tracing.enabled = true or clear the endpoint)")
	}
	if c.Tracing.Enabled && (c.Tracing.SampleRatio < 0.0 || c.Tracing.SampleRatio > 1.0) {
		return fmt.Errorf("config: tracing.sample_ratio %v out of range (must be between 0.0 and 1.0)", c.Tracing.SampleRatio)
	}
	if c.Autolink.Threshold < 0.0 || c.Autolink.Threshold > 1.0 {
		return fmt.Errorf("config: autolink.threshold %v out of range (must be between 0.0 and 1.0)", c.Autolink.Threshold)
	}
	if c.Autolink.TopK <= 0 {
		return fmt.Errorf("config: autolink.top_k %d must be greater than 0", c.Autolink.TopK)
	}
	if c.Blast.HubDegreeThreshold == 0 {
		return fmt.Errorf("config: blast.hub_degree_threshold must not be 0 (use a positive degree to gate, or a negative value to disable the hub gate)")
	}
	return nil
}
