package ports

// Line is a single source line paired with its 1-based line number. It is defined
// here so callers do not need to cross-import packages.
type Line struct {
	Number int
	Text   string
}

// ScanInput carries only the lines newly added by a change, restricting scanning
// to fresh content.
type ScanInput struct {
	AddedLines map[string][]Line
}

// SecretFinding represents a single secret-shaped value detected in scanned input.
type SecretFinding struct {
	Rule     string
	FilePath string
	Line     int

	// Redacted is the secret-shaped string with the sensitive value masked, which
	// is safe to surface in findings and logs.
	Redacted string

	// Confidence is the scanner's per-finding confidence in the range 0 to 1.
	Confidence float64
}

// SecretsScanner is the port for detecting secret-shaped values in newly-added
// source lines.
type SecretsScanner interface {
	Scan(in ScanInput) ([]SecretFinding, error)
}
