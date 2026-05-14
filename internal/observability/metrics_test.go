package observability_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/whiskeyjimbo/veska/internal/observability"
)

func TestNewMetrics_AllMetricsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
}

func TestMetrics_SealLatency(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.SealLatency.WithLabelValues("repo1").Observe(0.1)

	// CollectAndCount returns number of metric series collected.
	n := testutil.CollectAndCount(m.SealLatency)
	if n == 0 {
		t.Error("SealLatency: expected at least one series after Observe")
	}
}

func TestMetrics_PostCommitHookDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.PostCommitHookDuration.WithLabelValues("repo1", "typical").Observe(0.05)

	n := testutil.CollectAndCount(m.PostCommitHookDuration)
	if n == 0 {
		t.Error("PostCommitHookDuration: expected at least one series after Observe")
	}
}

func TestMetrics_MCPRequestsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.MCPRequestsTotal.WithLabelValues("eng_find_symbol", "ok").Inc()
	m.MCPRequestsTotal.WithLabelValues("eng_find_symbol", "ok").Inc()

	// CounterVec with a single label-set is a gauge/counter: ToFloat64 works.
	val := testutil.ToFloat64(m.MCPRequestsTotal.WithLabelValues("eng_find_symbol", "ok"))
	if val != 2 {
		t.Errorf("MCPRequestsTotal: got %v, want 2", val)
	}
}

func TestMetrics_MCPRequestDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.MCPRequestDuration.WithLabelValues("eng_find_symbol", "ok").Observe(0.02)

	n := testutil.CollectAndCount(m.MCPRequestDuration)
	if n == 0 {
		t.Error("MCPRequestDuration: expected at least one series after Observe")
	}
}

func TestMetrics_VectorQueryDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.VectorQueryDuration.WithLabelValues("semantic_search").Observe(0.003)

	n := testutil.CollectAndCount(m.VectorQueryDuration)
	if n == 0 {
		t.Error("VectorQueryDuration: expected at least one series after Observe")
	}
}

func TestMetrics_ErrorCount(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.ErrorCount.WithLabelValues("promotion").Inc()
	m.ErrorCount.WithLabelValues("embed").Inc()
	m.ErrorCount.WithLabelValues("embed").Inc()

	promVal := testutil.ToFloat64(m.ErrorCount.WithLabelValues("promotion"))
	embedVal := testutil.ToFloat64(m.ErrorCount.WithLabelValues("embed"))
	if promVal != 1 {
		t.Errorf("ErrorCount[promotion]: got %v, want 1", promVal)
	}
	if embedVal != 2 {
		t.Errorf("ErrorCount[embed]: got %v, want 2", embedVal)
	}
}

func TestStartHTTPListener_ServesMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	// Increment a counter so there's something to scrape.
	m.MCPRequestsTotal.WithLabelValues("eng_get_node", "ok").Inc()

	// Use httptest to bind a free port.
	ts := httptest.NewServer(observability.MetricsHandler(reg))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(bodyBytes)
	if !strings.Contains(body, "veska_mcp_requests_total") {
		t.Errorf("body does not contain veska_mcp_requests_total; body:\n%s", body)
	}
}

func TestStartHTTPListener_BindsAndCloses(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = observability.NewMetrics(reg)

	closer, err := observability.StartHTTPListener("127.0.0.1:0", reg)
	if err != nil {
		t.Fatalf("StartHTTPListener: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
