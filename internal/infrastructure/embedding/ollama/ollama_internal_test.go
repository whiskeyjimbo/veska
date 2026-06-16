package ollama

import (
	"net/http"
	"testing"
	"time"
)

// These white-box tests assert that WithTimeout and WithHTTPClient are
// order-independent functional options, per.

func TestOptions_TimeoutThenCustomClient_UsesCustomUnchanged(t *testing.T) {
	custom := &http.Client{Timeout: 2 * time.Second}
	p, err := New("m", WithTimeout(5*time.Second), WithHTTPClient(custom))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.client != custom {
		t.Fatalf("expected provider to use the custom client, got a different client")
	}
	if p.client.Timeout != 2*time.Second {
		t.Fatalf("custom client timeout = %v, want 2s (WithTimeout must be ignored)", p.client.Timeout)
	}
}

func TestOptions_CustomClientThenTimeout_SameResult(t *testing.T) {
	custom := &http.Client{Timeout: 2 * time.Second}
	p, err := New("m", WithHTTPClient(custom), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.client != custom {
		t.Fatalf("expected provider to use the custom client, got a different client")
	}
	if p.client.Timeout != 2*time.Second {
		t.Fatalf("custom client timeout = %v, want 2s (WithTimeout must be ignored)", p.client.Timeout)
	}
}

func TestOptions_BothOrderings_IdenticalConfig(t *testing.T) {
	c1 := &http.Client{Timeout: 2 * time.Second}
	c2 := &http.Client{Timeout: 2 * time.Second}
	a, err := New("m", WithTimeout(5*time.Second), WithHTTPClient(c1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := New("m", WithHTTPClient(c2), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.client.Timeout != b.client.Timeout {
		t.Fatalf("orderings diverge: a.timeout=%v b.timeout=%v", a.client.Timeout, b.client.Timeout)
	}
}

func TestOptions_TimeoutOnly_AppliesToDefaultClient(t *testing.T) {
	p, err := New("m", WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.client.Timeout != 5*time.Second {
		t.Fatalf("default client timeout = %v, want 5s", p.client.Timeout)
	}
}

func TestOptions_CustomClientNotMutated(t *testing.T) {
	custom := &http.Client{Timeout: 2 * time.Second}
	_, err := New("m", WithHTTPClient(custom), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if custom.Timeout != 2*time.Second {
		t.Fatalf("caller's client was mutated: timeout = %v, want 2s", custom.Timeout)
	}
}

func TestOptions_NoClientOptions_DefaultTimeout(t *testing.T) {
	p, err := New("m")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.client.Timeout != defaultTimeout {
		t.Fatalf("default client timeout = %v, want %v", p.client.Timeout, defaultTimeout)
	}
}
