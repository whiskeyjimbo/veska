// SPDX-License-Identifier: AGPL-3.0-only

// Package webui holds the embedded read-only graph viewer assets served by
// `veska graph serve`. The viewer is a single static page plus a vendored
// copy of Cytoscape.js (MIT, see static/cytoscape.min.js header); it fetches
// the graph snapshot from the server's /api/graph endpoint and renders it.
// Embedding the assets in the binary keeps `veska graph serve` self-contained
// and offline - no CDN, no network at view time.
package webui

import "embed"

// Assets holds the viewer's static files (index.html + static/*). Served
// read-only; there are no write endpoints.
//
//go:embed index.html static
var Assets embed.FS
