package doctor_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/doctor"
)

func TestCheckEgressObservability_NoListeners(t *testing.T) {
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		MetricsListener: "",
		OTLPEndpoint:    "",
	})

	if len(report.Destinations) != 0 {
		t.Errorf("expected 0 destinations when nothing is configured, got %d", len(report.Destinations))
	}
}

func TestCheckEgressObservability_MetricsOnly(t *testing.T) {
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		MetricsListener:      "127.0.0.1:9090",
		MetricsConfiguredVia: "config:metrics.listen",
		OTLPEndpoint:         "",
	})

	if len(report.Destinations) != 1 {
		t.Fatalf("expected 1 destination, got %d", len(report.Destinations))
	}
	d := report.Destinations[0]
	if d.Kind != "metrics" {
		t.Errorf("kind: got %q, want %q", d.Kind, "metrics")
	}
	if d.Listen != "127.0.0.1:9090" {
		t.Errorf("listen: got %q, want %q", d.Listen, "127.0.0.1:9090")
	}
	if d.ConfiguredVia != "config:metrics.listen" {
		t.Errorf("configured_via: got %q, want %q", d.ConfiguredVia, "config:metrics.listen")
	}
}

func TestCheckEgressObservability_OTLPOnly(t *testing.T) {
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		MetricsListener:   "",
		OTLPEndpoint:      "http://otel.local:4317",
		OTLPConfiguredVia: "VESKA_OTLP_ENDPOINT",
	})

	if len(report.Destinations) != 1 {
		t.Fatalf("expected 1 destination, got %d", len(report.Destinations))
	}
	d := report.Destinations[0]
	if d.Kind != "otlp" {
		t.Errorf("kind: got %q, want %q", d.Kind, "otlp")
	}
	if d.URL != "http://otel.local:4317" {
		t.Errorf("url: got %q, want %q", d.URL, "http://otel.local:4317")
	}
	if d.ConfiguredVia != "VESKA_OTLP_ENDPOINT" {
		t.Errorf("configured_via: got %q, want %q", d.ConfiguredVia, "VESKA_OTLP_ENDPOINT")
	}
}

func TestCheckEgressObservability_BothConfigured(t *testing.T) {
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		MetricsListener:      "127.0.0.1:9090",
		MetricsConfiguredVia: "config:metrics.listen",
		OTLPEndpoint:         "http://otel.local:4317",
		OTLPConfiguredVia:    "VESKA_OTLP_ENDPOINT",
	})

	if len(report.Destinations) != 2 {
		t.Fatalf("expected 2 destinations, got %d", len(report.Destinations))
	}

	kinds := make(map[string]bool)
	for _, d := range report.Destinations {
		kinds[d.Kind] = true
	}
	if !kinds["metrics"] {
		t.Error("missing 'metrics' destination")
	}
	if !kinds["otlp"] {
		t.Error("missing 'otlp' destination")
	}
}

func TestCheckEgressObservability_ReviewLLMEnabled(t *testing.T) {
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		ReviewLLMEndpoint:      "http://127.0.0.1:11434",
		ReviewLLMConfiguredVia: "config:llm_generator.endpoint",
	})

	if len(report.Destinations) != 1 {
		t.Fatalf("expected 1 destination, got %d", len(report.Destinations))
	}
	d := report.Destinations[0]
	if d.Kind != "review_llm" {
		t.Errorf("kind: got %q, want %q", d.Kind, "review_llm")
	}
	if d.URL != "http://127.0.0.1:11434" {
		t.Errorf("url: got %q, want %q", d.URL, "http://127.0.0.1:11434")
	}
	if d.ConfiguredVia != "config:llm_generator.endpoint" {
		t.Errorf("configured_via: got %q, want %q", d.ConfiguredVia, "config:llm_generator.endpoint")
	}
}

func TestCheckEgressObservability_VulnSourceConfigured(t *testing.T) {
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		VulnSourceEndpoint:      "https://osv-vulnerabilities.storage.googleapis.com/Go/all.zip",
		VulnSourceConfiguredVia: "config:vuln_source.provider",
	})

	if len(report.Destinations) != 1 {
		t.Fatalf("expected 1 destination, got %d", len(report.Destinations))
	}
	d := report.Destinations[0]
	if d.Kind != "vuln_source" {
		t.Errorf("kind: got %q, want %q", d.Kind, "vuln_source")
	}
	if d.URL != "https://osv-vulnerabilities.storage.googleapis.com/Go/all.zip" {
		t.Errorf("url: got %q", d.URL)
	}
	if d.ConfiguredVia != "config:vuln_source.provider" {
		t.Errorf("configured_via: got %q, want %q", d.ConfiguredVia, "config:vuln_source.provider")
	}
}

func TestCheckEgressObservability_VulnSourceDisabledOmitted(t *testing.T) {
	// Caller passes an empty endpoint when [vuln_source] is absent, so the
	// vuln_source destination must not appear.
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		VulnSourceEndpoint: "",
	})

	for _, d := range report.Destinations {
		if d.Kind == "vuln_source" {
			t.Errorf("vuln_source destination present when feature off: %+v", d)
		}
	}
}

func TestCheckEgressObservability_ReviewLLMDisabledOmitted(t *testing.T) {
	// Caller passes an empty endpoint when review.enabled is false, so the
	// review LLM destination must not appear even though Ollama is configured.
	report := doctor.CheckEgressObservability(doctor.EgressObservabilityParams{
		ReviewLLMEndpoint: "",
	})

	for _, d := range report.Destinations {
		if d.Kind == "review_llm" {
			t.Errorf("review_llm destination present when review disabled: %+v", d)
		}
	}
}
