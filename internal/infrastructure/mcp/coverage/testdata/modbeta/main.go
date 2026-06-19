// SPDX-License-Identifier: AGPL-3.0-only

// Command modbeta serves the beta dashboard. It is the program entry point
// (eng_get_entry_points must surface main) and registers one HTTP handler.
package main

import (
	"fmt"
	"net/http"

	"example.com/modbeta/widget"
)

func main() {
	http.HandleFunc("/badge", badgeHandler)
	// TODO: make the listen address configurable via flag.
	_ = http.ListenAndServe(":8080", nil)
}

// badgeHandler renders a badge widget and writes it to the response.
func badgeHandler(w http.ResponseWriter, r *http.Request) {
	b := widget.Badge{Label: "ok", Palette: widget.Palette{Theme: "dark"}}
	fmt.Fprint(w, b.RenderBadge([]float64{1, 2, 3}))
}
