package main

import (
	"testing"
)

func TestRootCmdHelp(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("expected nil error from --help, got: %v", err)
	}
}
