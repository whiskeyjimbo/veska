// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// checkLLMProvider gates daemon startup on the review-pipeline LLM provider.
// Only the local Ollama provider is supported in V2.0; hosted providers
// (anthropic, openai, gemini) are deferred to V2.0.1. A configured provider
// other than "ollama" (an empty value defaults to ollama) is a fatal startup
// error so an operator does not silently run with an unimplemented backend.
func checkLLMProvider(cfg config.Config) error {
	provider := cfg.LLMGenerator.Provider
	if provider == "" || provider == "ollama" {
		return nil
	}
	return fmt.Errorf(
		"daemon: llm_generator.provider %q is not supported: only 'ollama' is available in V2.0; "+
			"hosted providers (anthropic, openai, gemini) are deferred to V2.0.1",
		provider,
	)
}
