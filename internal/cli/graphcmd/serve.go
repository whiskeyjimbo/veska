// SPDX-License-Identifier: AGPL-3.0-only

package graphcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/graphexport"
	"github.com/whiskeyjimbo/veska/internal/cli/graphcmd/webui"
)

// ServeParams bundles the inputs of RunServe.
type ServeParams struct {
	// SnapshotPath is an optional committed snapshot file to serve. When set
	// (and Live is false) the server serves it verbatim - no daemon, no DB.
	SnapshotPath string
	// Live forces a fresh in-process export of the live DB even when a
	// SnapshotPath is given.
	Live bool
	// RepoArg / Branch select the repo/branch for the live export; ignored
	// when serving a snapshot file.
	RepoArg string
	Branch  string
	// Addr is the localhost bind address (host:port). Defaults to
	// 127.0.0.1:8744.
	Addr string
	Out  io.Writer
}

// RunServe implements `veska graph serve [snapshot.json]`. It starts a
// read-only HTTP server bound to localhost that serves the embedded graph
// viewer and the graph snapshot as JSON. The snapshot is either read from the
// given file or freshly exported in-process from the live DB (same export
// function as `graph export`). The server has no write/mutate endpoints.
func RunServe(ctx context.Context, p ServeParams) error {
	addr := p.Addr
	if addr == "" {
		addr = "127.0.0.1:8744"
	}

	data, label, err := snapshotBytes(ctx, p)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           viewerHandler(data),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("graph serve: bind %s: %w (is another viewer running? pass --addr)", addr, err)
	}

	fmt.Fprintf(p.Out, "serving %s at http://%s (read-only; Ctrl-C to stop)\n", label, ln.Addr().String())

	// Shut down cleanly when the context is cancelled (Ctrl-C).
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("graph serve: %w", err)
	}
	return nil
}

// viewerHandler builds the read-only HTTP handler: /api/graph serves the
// snapshot JSON, /static/* serves the embedded viewer assets, and / serves the
// viewer page. Wrapped in readOnly so only GET/HEAD reach any route.
func viewerHandler(data []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/graph", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	mux.Handle("/static/", http.FileServer(http.FS(webui.Assets)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		index, rerr := webui.Assets.ReadFile("index.html")
		if rerr != nil {
			http.Error(w, "viewer unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
	return readOnly(mux)
}

// snapshotBytes resolves the JSON payload to serve plus a human label for the
// startup line: the committed file's bytes, or a fresh in-process export.
func snapshotBytes(ctx context.Context, p ServeParams) (data []byte, label string, err error) {
	if p.SnapshotPath != "" && !p.Live {
		b, rerr := os.ReadFile(p.SnapshotPath)
		if rerr != nil {
			return nil, "", fmt.Errorf("graph serve: read snapshot %s: %w", p.SnapshotPath, rerr)
		}
		return b, p.SnapshotPath, nil
	}
	// The live export ranks hot zones / entry points over the whole graph and
	// can take tens of seconds on a large repo; tell the user so the wait
	// before the server binds isn't a silent stall.
	fmt.Fprintln(p.Out, "exporting live graph snapshot (this can take a moment on large repos)…")
	snap, repoID, berr := buildSnapshot(ctx, p.RepoArg, p.Branch)
	if berr != nil {
		return nil, "", berr
	}
	b, merr := graphexport.Marshal(snap)
	if merr != nil {
		return nil, "", fmt.Errorf("graph serve: %w", merr)
	}
	return b, "live export of " + repoID[:min(12, len(repoID))], nil
}

// readOnly rejects any non-GET/HEAD request so the server can never mutate
// state (AC4). The viewer only ever issues GETs.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only viewer", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}
