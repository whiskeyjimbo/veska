package ports

// Line is a single source line paired with its 1-based line number. This is a
// field-identical copy of the Line types in the git, application, and checks
// packages — mirrored here so callers need not cross-import, consistent with
// how Dependency and VulnFinding are kept self-contained in this package.
type Line struct {
	// Number is the 1-based line number within the file.
	Number int

	// Text is the verbatim content of the line.
	Text string
}

// ScanInput is the request passed to a SecretsScanner. It carries only the
// lines newly added by a change, so scanning is confined to fresh content
// rather than the whole file.
type ScanInput struct {
	// AddedLines maps a file path to the lines newly added in that file.
	AddedLines map[string][]Line
}

// SecretFinding represents a single secret-shaped value detected in scanned
// input. Fields mirror the minimal set needed by application-layer callers.
type SecretFinding struct {
	// Rule is the name of the detection rule that matched.
	Rule string

	// FilePath is the path of the file the secret was found in.
	FilePath string

	// Line is the 1-based line number of the matching line.
	Line int

	// Redacted is the secret-shaped string with the sensitive value masked,
	// safe to surface in findings and logs.
	Redacted string

	// Confidence is the scanner's per-finding confidence in the range 0..1.
	Confidence float64
}

// SecretsScanner is the port for detecting secret-shaped values in newly-added
// source lines. Implementations are provided by infrastructure adapters.
type SecretsScanner interface {
	// Scan inspects the added lines in the input and returns any secret
	// findings. An empty slice and a nil error means nothing matched.
	Scan(in ScanInput) ([]SecretFinding, error)
}
