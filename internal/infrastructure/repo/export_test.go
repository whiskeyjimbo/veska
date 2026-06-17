package repo

// ResolveVeskaBinaryForTest exposes the internal resolveVeskaBinary helper to package tests.
func ResolveVeskaBinaryForTest(exe string) string { return resolveVeskaBinary(exe) }
