package mcp

import (
	"database/sql"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RegisterFindingTools registers tools for managing findings, where passing a nil AuditWriter disables auditing.
func RegisterFindingTools(r *Registry, db *sql.DB, aw ports.AuditWriter, repos application.RepoLister) {
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
		InputSchema:     listFindingsInputSchema,
		Handler:         makeListFindingsHandler(db, repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_reopen_finding",
		Description:     "Reopen a previously closed finding by ID.",
		IncludesStaging: false,
		InputSchema:     reopenFindingInputSchema,
		Handler:         makeReopenFindingHandler(db, aw),
	})
}
