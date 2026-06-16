package doctor

// EgressDestination describes one active outbound endpoint or listener.
// Matches the egress schema.
type EgressDestination struct {
	// Kind is one of: "metrics", "otlp", "review_llm", "vuln_source".
	Kind string `json:"kind"`
	// Listen is set for HTTP listeners (metrics). Empty for dial-out endpoints.
	Listen string `json:"listen,omitempty"`
	// URL is set for dial-out endpoints (otlp, review_llm, vuln_source).
	// Empty for listeners.
	URL string `json:"url,omitempty"`
	// ConfiguredVia cites the provenance: "default" | env var name | "config:<key>".
	ConfiguredVia string `json:"configured_via"`
}

// EgressObservabilityReport enumerates active HTTP listeners and OTLP exporters.
// Unset destinations are omitted from Destinations (not represented as null).
type EgressObservabilityReport struct {
	Destinations []EgressDestination `json:"destinations"`
}

// EgressObservabilityParams carries the runtime configuration needed to build
// the observability egress report. Empty strings mean "not configured".
type EgressObservabilityParams struct {
	// MetricsListener is the bound listen address (e.g. "127.0.0.1:9090").
	// Empty means metrics HTTP listener is not active.
	MetricsListener string
	// MetricsConfiguredVia cites how MetricsListener was set.
	MetricsConfiguredVia string

	// OTLPEndpoint is the collector endpoint (e.g. "http://otel.local:4317").
	// Empty means OTLP exporter is not active.
	OTLPEndpoint string
	// OTLPConfiguredVia cites how OTLPEndpoint was set.
	OTLPConfiguredVia string

	// ReviewLLMEndpoint is the review pipeline's LLM endpoint (e.g. the local
	// Ollama endpoint "http://127.0.0.1:11434"). The caller passes "" when the
	// review pipeline is disabled, so the destination is omitted.
	ReviewLLMEndpoint string
	// ReviewLLMConfiguredVia cites how ReviewLLMEndpoint was set.
	ReviewLLMConfiguredVia string

	// VulnSourceEndpoint is the vulnerability advisory source's dump URL (the
	// OSV.dev Go-ecosystem dump). The caller passes "" when [vuln_source] is
	// not configured, so the destination is omitted.
	VulnSourceEndpoint string
	// VulnSourceConfiguredVia cites how VulnSourceEndpoint was set.
	VulnSourceConfiguredVia string
}

// CheckEgressObservability builds an EgressObservabilityReport from the provided
// params. It never returns an error - it purely projects configuration state into
// the report shape.
func CheckEgressObservability(params EgressObservabilityParams) EgressObservabilityReport {
	dests := make([]EgressDestination, 0, 4)

	if params.MetricsListener != "" {
		via := params.MetricsConfiguredVia
		if via == "" {
			via = "default"
		}
		dests = append(dests, EgressDestination{
			Kind:          "metrics",
			Listen:        params.MetricsListener,
			ConfiguredVia: via,
		})
	}

	if params.OTLPEndpoint != "" {
		via := params.OTLPConfiguredVia
		if via == "" {
			via = "default"
		}
		dests = append(dests, EgressDestination{
			Kind:          "otlp",
			URL:           params.OTLPEndpoint,
			ConfiguredVia: via,
		})
	}

	if params.ReviewLLMEndpoint != "" {
		via := params.ReviewLLMConfiguredVia
		if via == "" {
			via = "default"
		}
		dests = append(dests, EgressDestination{
			Kind:          "review_llm",
			URL:           params.ReviewLLMEndpoint,
			ConfiguredVia: via,
		})
	}

	if params.VulnSourceEndpoint != "" {
		via := params.VulnSourceConfiguredVia
		if via == "" {
			via = "default"
		}
		dests = append(dests, EgressDestination{
			Kind:          "vuln_source",
			URL:           params.VulnSourceEndpoint,
			ConfiguredVia: via,
		})
	}

	return EgressObservabilityReport{Destinations: dests}
}
