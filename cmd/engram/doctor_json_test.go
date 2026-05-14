package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// captureJSONOutput executes cmd with --json and returns the parsed envelope.
func captureJSONOutput(cmd *cobra.Command) (map[string]any, error) {
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// Inject --json flag value; the flag is already registered on each sub-cmd.
	if err := cmd.Flags().Set("json", "true"); err != nil {
		return nil, err
	}
	if err := cmd.Execute(); err != nil {
		// ProbeStatusError is acceptable — envelope should still be present.
		if _, ok := err.(ProbeStatusError); !ok {
			return nil, err
		}
	}
	var result map[string]any
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		return nil, err
	}
	return result, nil
}

// assertEnvelope validates required top-level envelope fields.
func assertEnvelope(t *testing.T, subsystem string, m map[string]any) {
	t.Helper()

	// schema_version must be float64(1) after JSON unmarshal.
	sv, ok := m["schema_version"]
	if !ok {
		t.Errorf("[%s] missing field schema_version", subsystem)
	} else if sv != float64(1) {
		t.Errorf("[%s] schema_version: want 1, got %v", subsystem, sv)
	}

	// subsystem must match.
	ss, ok := m["subsystem"]
	if !ok {
		t.Errorf("[%s] missing field subsystem", subsystem)
	} else if ss != subsystem {
		t.Errorf("[%s] subsystem: want %q, got %q", subsystem, subsystem, ss)
	}

	// status must be one of healthy|degraded|broken.
	status, ok := m["status"]
	if !ok {
		t.Errorf("[%s] missing field status", subsystem)
	} else {
		switch status {
		case "healthy", "degraded", "broken":
		default:
			t.Errorf("[%s] status: got unexpected value %q", subsystem, status)
		}
	}

	// ts must be a valid RFC3339 string.
	ts, ok := m["ts"]
	if !ok {
		t.Errorf("[%s] missing field ts", subsystem)
	} else {
		tsStr, isStr := ts.(string)
		if !isStr {
			t.Errorf("[%s] ts: expected string, got %T", subsystem, ts)
		} else if _, err := time.Parse(time.RFC3339, tsStr); err != nil {
			t.Errorf("[%s] ts: not valid RFC3339: %q (%v)", subsystem, tsStr, err)
		}
	}

	// data must be present.
	if _, ok := m["data"]; !ok {
		t.Errorf("[%s] missing field data", subsystem)
	}
}

func TestDoctorJSONEnvelopeStorage(t *testing.T) {
	cmd := doctorStorageCmd()
	m, err := captureJSONOutput(cmd)
	if err != nil {
		t.Fatalf("storage --json: %v", err)
	}
	assertEnvelope(t, "storage", m)
}

func TestDoctorJSONEnvelopeEmbedder(t *testing.T) {
	cmd := doctorEmbedderCmd()
	m, err := captureJSONOutput(cmd)
	if err != nil {
		t.Fatalf("embedder --json: %v", err)
	}
	assertEnvelope(t, "embedder", m)
}

func TestDoctorJSONEnvelopeEgress(t *testing.T) {
	cmd := doctorEgressCmd()
	m, err := captureJSONOutput(cmd)
	if err != nil {
		t.Fatalf("egress --json: %v", err)
	}
	assertEnvelope(t, "egress", m)
}

func TestDoctorJSONEnvelopeConfig(t *testing.T) {
	cmd := doctorConfigCmd()
	m, err := captureJSONOutput(cmd)
	if err != nil {
		t.Fatalf("config --json: %v", err)
	}
	assertEnvelope(t, "config", m)
}

func TestDoctorJSONEnvelopeStatus(t *testing.T) {
	cmd := doctorStatusCmd()
	m, err := captureJSONOutput(cmd)
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	assertEnvelope(t, "status", m)
}

func TestDoctorJSONEnvelopeStub(t *testing.T) {
	// post_promotion_queue is a stub — data should be an empty object.
	cmd := doctorSubCmd("post_promotion_queue", "stub", func(jsonOut bool, w io.Writer) error {
		return stubOK("post_promotion_queue", jsonOut, w)
	})
	m, err := captureJSONOutput(cmd)
	if err != nil {
		t.Fatalf("post_promotion_queue --json: %v", err)
	}
	assertEnvelope(t, "post_promotion_queue", m)

	// data must be an empty map (not nil, not a non-object).
	dataVal, ok := m["data"]
	if !ok {
		t.Fatal("post_promotion_queue --json: missing data field")
	}
	dataMap, ok := dataVal.(map[string]any)
	if !ok {
		t.Fatalf("post_promotion_queue --json: data should be object, got %T", dataVal)
	}
	if len(dataMap) != 0 {
		t.Errorf("post_promotion_queue --json: expected empty data object, got %v", dataMap)
	}
}

func TestDoctorJSONEnvelopeNoExtraTopLevel(t *testing.T) {
	// The envelope must not leak raw probe fields at the top level.
	cmd := doctorStorageCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Execute()

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// These are raw StorageReport fields that must NOT appear at the top level.
	forbidden := []string{"engram_home", "db_path", "db_size_bytes", "free_ratio"}
	for _, f := range forbidden {
		if _, found := m[f]; found {
			t.Errorf("raw field %q must be inside data, not at top level", f)
		}
	}

	// Validate data contains the StorageReport fields.
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatal("data field is not an object")
	}
	for _, f := range forbidden {
		if _, found := data[f]; !found {
			t.Errorf("data object missing expected StorageReport field %q", f)
		}
	}
}

func TestDoctorJSONStatusRollupInsideData(t *testing.T) {
	// The status rollup per-subsystem breakdown should live inside data.
	cmd := doctorStatusCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Execute()

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatal("status data is not a JSON object")
	}

	for _, key := range []string{"embedder", "egress", "config"} {
		if _, found := data[key]; !found {
			t.Errorf("status data missing subsystem key %q", key)
		}
	}

	// The raw "status" field at the top level should be the rollup status string.
	status, ok := m["status"].(string)
	if !ok || !strings.Contains("healthy degraded broken", status) {
		t.Errorf("top-level status unexpected: %v", m["status"])
	}
}
