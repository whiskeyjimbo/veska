package ports

import "context"

// ExportedSymbol is the minimal projection the breaking-removal diff gate
// (solov2-zvh6.12) needs to decide whether an exported public-surface symbol
// present at base-ref has disappeared from the candidate. It carries node_id
// for naming/anchoring and the (file_path, kind, name) the gate folds into a
// package-scoped identity key (package = path.Dir(file_path)) so an
// intra-package file move is NOT mistaken for a removal.
type ExportedSymbol struct {
	NodeID   string
	FilePath string
	Kind     string
	Name     string
}

// ExportedSymbolQuerier is the read-side port the breaking-removal gate uses to
// enumerate exported public-surface symbols over a set of changed files.
//
// ExportedSymbolsInFiles returns the nodes in (repoID, branch) whose file_path
// is in filePaths, whose exported flag is true, and whose kind is one of
// {function, method, interface} (the same closed public-surface set the
// contract-drift gate judges). An empty filePaths slice MUST return an empty
// result with no error — symmetric with ContractDriftQuerier / DeadCodeQuerier,
// and it avoids a degenerate "IN ()" clause at the adapter.
type ExportedSymbolQuerier interface {
	ExportedSymbolsInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]ExportedSymbol, error)
}
