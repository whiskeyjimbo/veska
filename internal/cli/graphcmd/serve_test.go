// SPDX-License-Identifier: AGPL-3.0-only

package graphcmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestViewerHandler_ServesSnapshotAndAssets(t *testing.T) {
	payload := []byte(`{"schema_version":1,"nodes":[]}`)
	srv := httptest.NewServer(viewerHandler(payload))
	defer srv.Close()

	cases := []struct {
		path        string
		wantStatus  int
		wantCTHas   string
		wantBodyHas string
	}{
		{"/api/graph", http.StatusOK, "application/json", `"schema_version":1`},
		{"/", http.StatusOK, "text/html", "<div id=\"cy\">"},
		{"/static/app.js", http.StatusOK, "javascript", "/api/graph"},
		{"/static/cytoscape.min.js", http.StatusOK, "", "cytoscape"},
		{"/nope", http.StatusNotFound, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			if tc.wantCTHas != "" && !strings.Contains(resp.Header.Get("Content-Type"), tc.wantCTHas) {
				t.Errorf("content-type = %q, want contains %q", resp.Header.Get("Content-Type"), tc.wantCTHas)
			}
			body, _ := io.ReadAll(resp.Body)
			if tc.wantBodyHas != "" && !strings.Contains(string(body), tc.wantBodyHas) {
				t.Errorf("body missing %q", tc.wantBodyHas)
			}
		})
	}
}

// TestViewerHandler_ReadOnly is the AC4 gate: every mutating method is rejected
// before reaching any route.
func TestViewerHandler_ReadOnly(t *testing.T) {
	srv := httptest.NewServer(viewerHandler([]byte(`{}`)))
	defer srv.Close()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, srv.URL+"/api/graph", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
		}
	}
}
