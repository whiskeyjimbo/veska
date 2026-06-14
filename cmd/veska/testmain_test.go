package main

import (
	"os"
	"testing"
)

// TestMain isolates the whole cmd/veska test package from the developer's real
// VESKA_HOME. It points VESKA_HOME at a throwaway temp dir so no test reads,
// migrates, or pollutes ~/.veska.
//
// solov2-dchd surfaced why this matters: editing an already-applied migration
// in place (0019) makes the integrity check tamper-fail on any pre-existing DB,
// and OpenWithOptions responds with os.Exit(78) — so a single storage-opening
// test against a stale ~/.veska aborts the entire test binary (no clean
// `--- FAIL`). Defaulting VESKA_HOME to a fresh temp dir makes every test
// hermetic; a fresh DB migrates with the current SHA, so there is nothing to
// tamper against. Tests that need a specific home still call t.Setenv, which
// overrides this default for their duration and restores it after.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "veska-cmd-test-home-*")
	if err != nil {
		panic("create temp VESKA_HOME: " + err.Error())
	}
	if err := os.Setenv("VESKA_HOME", home); err != nil {
		panic("set VESKA_HOME: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
