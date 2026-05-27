package daemon

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/config"
)

// AC3: a non-ollama provider is refused with a message naming the V2.0.1
// deferral for hosted providers.
func TestCheckLLMProvider_RejectsHostedProvider(t *testing.T) {
	t.Parallel()

	for _, prov := range []string{"anthropic", "openai", "gemini"} {
		cfg := config.DefaultConfig()
		cfg.LLMGenerator.Provider = prov
		err := checkLLMProvider(cfg)
		if err == nil {
			t.Fatalf("provider %q: expected error, got nil", prov)
		}
		if !strings.Contains(err.Error(), "V2.0.1") {
			t.Errorf("provider %q: error %q does not name the V2.0.1 deferral", prov, err)
		}
	}
}

// The default provider (ollama) is accepted.
func TestCheckLLMProvider_AcceptsOllama(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	if cfg.LLMGenerator.Provider != "ollama" {
		t.Fatalf("precondition: default provider = %q, want ollama", cfg.LLMGenerator.Provider)
	}
	if err := checkLLMProvider(cfg); err != nil {
		t.Fatalf("ollama provider rejected: %v", err)
	}
}

// An empty provider is treated as the default (ollama) and accepted.
func TestCheckLLMProvider_EmptyIsOllama(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.LLMGenerator.Provider = ""
	if err := checkLLMProvider(cfg); err != nil {
		t.Fatalf("empty provider rejected: %v", err)
	}
}
