package domain

// ParseResult is the output of CodeParser.ParseFile.
// It carries the Nodes and Edges extracted from a single source file.
// Both slices may be nil or empty when the file contains no recognisable symbols.
type ParseResult struct {
	Nodes []*Node
	Edges []*Edge
}
