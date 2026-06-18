// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repo

// ResolveVeskaBinaryForTest exposes the internal resolveVeskaBinary helper to package tests.
func ResolveVeskaBinaryForTest(exe string) string { return resolveVeskaBinary(exe) }
