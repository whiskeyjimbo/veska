package mcp

import (
	"database/sql"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RegisterFindingTools registers finding management tools on r.
// db is the SQLite connection that backs the findings table.
// aw is an optional AuditWriter; pass nil to disable audit logging.
func RegisterFindingTools(r *Registry, db *sql.DB, aw ports.AuditWriter) {
	r.MustRegister(ToolSpec{
		Name:            "eng_close_finding",
		Description:     "Close a finding by ID. Severity >= high requires a human actor.",
		IncludesStaging: false,
		Handler:         makeCloseFindingHandler(db, aw),
		InputSchema:     closeFindingInputSchema,
		OutputSchema:    closeFindingOutputSchema,
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_list_findings",
		Description:     "List findings for a repo and branch, optionally filtered by state or severity.",
		IncludesStaging: false,
		Handler:         makeListFindingsHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_reopen_finding",
		Description:     "Reopen a previously closed finding by ID.",
		IncludesStaging: false,
		Handler:         makeReopenFindingHandler(db, aw),
	})
}
