// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"testing"
)

func TestScanTodos_FindsCommonMarkers(t *testing.T) {
	src := []byte(`package x

// TODO: refactor this
func foo() {}

/* FIXME: bug here */
func bar() {}

// note: not a todo
func baz() {}

// XXX really
func qux() {}
`)
	got := scanTodos(src)
	if len(got) != 3 {
		t.Fatalf("expected 3 todos, got %d (%+v)", len(got), got)
	}
	if got[0].Line != 3 {
		t.Errorf("first todo on line %d, want 3", got[0].Line)
	}
	if got[1].Line != 6 {
		t.Errorf("second todo on line %d, want 6", got[1].Line)
	}
	if got[2].Line != 12 {
		t.Errorf("third todo on line %d, want 12", got[2].Line)
	}
}

func TestScanTodos_StripsBlockClosers(t *testing.T) {
	src := []byte("/* TODO: foo */\n")
	got := scanTodos(src)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].Message != "TODO: foo" {
		t.Errorf("message = %q want %q", got[0].Message, "TODO: foo")
	}
}

func TestScanTodos_HtmlCommentClosers(t *testing.T) {
	src := []byte("<!-- TODO: html stuff -->\n")
	got := scanTodos(src)
	if len(got) != 1 || got[0].Message != "TODO: html stuff" {
		t.Fatalf("got %+v", got)
	}
}

func TestScanTodos_RejectsTodoEmbeddedInWord(t *testing.T) {
	src := []byte("// TODOLIST is not a todo\n")
	got := scanTodos(src)
	if len(got) != 0 {
		t.Errorf("expected no todos for word-embedded TODO, got %+v", got)
	}
}

func TestScanTodos_ShellAndScriptStyle(t *testing.T) {
	src := []byte(`#!/bin/sh
# TODO shell todo
echo hi
`)
	got := scanTodos(src)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %+v", got)
	}
}

func TestScanTodos_EmptySrc(t *testing.T) {
	if got := scanTodos(nil); got != nil {
		t.Errorf("expected nil for empty src, got %+v", got)
	}
}
