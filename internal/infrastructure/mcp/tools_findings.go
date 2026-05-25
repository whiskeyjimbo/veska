package mcp

import (
	"database/sql"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RegisterFindingTools registers finding management tools on r.
// db is the SQLite connection that backs the findings table.
// aw is an optional AuditWriter; pass nil to disable audit logging.
// repos is used by eng_list_findings to fall back to a cwd-injected
// repo_id when the caller omits it (solov2-ig2x); pass nil to disable
// the fallback (the older "repo_id is required" behaviour is preserved).
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
