// SPDX-License-Identifier: AGPL-3.0-only

package doctor_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

func TestCheckEmbedderHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text"}]}`))
	}))
	defer srv.Close()

	report, err := doctor.CheckEmbedder(srv.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("CheckEmbedder: unexpected error: %v", err)
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want %q", report.Status, "healthy")
	}
	if report.OllamaURL != srv.URL {
		t.Errorf("OllamaURL: got %q, want %q", report.OllamaURL, srv.URL)
	}
	if report.ModelName != "nomic-embed-text" {
		t.Errorf("ModelName: got %q, want %q", report.ModelName, "nomic-embed-text")
	}
}

func TestCheckEmbedderDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3"}]}`))
	}))
	defer srv.Close()

	report, err := doctor.CheckEmbedder(srv.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("CheckEmbedder: unexpected error: %v", err)
	}
	if report.Status != "degraded" {
		t.Errorf("Status: got %q, want %q", report.Status, "degraded")
	}
}

func TestCheckEmbedderBroken(t *testing.T) {
	report, err := doctor.CheckEmbedder("http://127.0.0.1:19999", "nomic-embed-text")
	if err != nil {
		t.Fatalf("CheckEmbedder: unexpected error: %v", err)
	}
	if report.Status != "broken" {
		t.Errorf("Status: got %q, want %q", report.Status, "broken")
	}
}
