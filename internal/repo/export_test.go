package repo

// ResolveVeskaBinaryForTest is an export-for-test of the pure
// path-shaping helper inside veskaBinary, so unit tests can exercise
// the "running binary is veska-daemon → resolve sibling veska CLI"
// behaviour without depending on os.Executable.
func ResolveVeskaBinaryForTest(exe string) string { return resolveVeskaBinary(exe) }
