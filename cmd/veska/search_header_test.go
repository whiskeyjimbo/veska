package main

import (
	"bytes"
	"strings"
	"testing"
)

// solov2-izh6.15: the user-facing 'searching:' header must make the
// repo scope visible above the results so a cwd-scoped query doesn't
// silently drop other registered repos.

func TestEmitSearchHeader_CwdScopedUsesAliasOrShortID(t *testing.T) {
	var stderr, stdout bytes.Buffer
	emitSearchHeader(&stderr, &stdout, false, searchHeaderInfo{
		mode:    searchHeaderModeCwd,
		repoID:  "52d6c257dfe2abcdef0011223344556677889900aabbccddeeff001122334455",
		shortID: "52d6c257dfe2",
		aliases: []string{"greetcli"},
	})
	got := stderr.String()
	if !strings.Contains(got, "searching:") {
		t.Errorf("expected stderr to contain 'searching:'; got %q", got)
	}
	if !strings.Contains(got, "greetcli") {
		t.Errorf("expected stderr to surface alias 'greetcli'; got %q", got)
	}
	if !strings.Contains(got, "--repo") {
		t.Errorf("expected hint mentioning --repo override; got %q", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("header must not write to stdout; got %q", stdout.String())
	}
}

func TestEmitSearchHeader_CwdScopedFallsBackToShortID(t *testing.T) {
	var stderr, stdout bytes.Buffer
	emitSearchHeader(&stderr, &stdout, false, searchHeaderInfo{
		mode:    searchHeaderModeCwd,
		repoID:  "52d6c257dfe2abcdef0011223344556677889900aabbccddeeff001122334455",
		shortID: "52d6c257dfe2",
	})
	got := stderr.String()
	if !strings.Contains(got, "52d6c257dfe2") {
		t.Errorf("expected stderr to surface short_id when no alias; got %q", got)
	}
}

func TestEmitSearchHeader_ExplicitRepoNoOverrideHint(t *testing.T) {
	var stderr, stdout bytes.Buffer
	emitSearchHeader(&stderr, &stdout, false, searchHeaderInfo{
		mode:    searchHeaderModeExplicit,
		repoID:  "0e17bc277263abcdef0011223344556677889900aabbccddeeff001122334455",
		shortID: "0e17bc277263",
		aliases: []string{"greetlib"},
	})
	got := stderr.String()
	if !strings.Contains(got, "searching:") || !strings.Contains(got, "greetlib") {
		t.Errorf("explicit header should mention repo; got %q", got)
	}
	if strings.Contains(got, "--repo to override") {
		t.Errorf("explicit-mode header should NOT advise --repo override (user already specified); got %q", got)
	}
}

func TestEmitSearchHeader_FanoutAllRepos(t *testing.T) {
	var stderr, stdout bytes.Buffer
	emitSearchHeader(&stderr, &stdout, false, searchHeaderInfo{
		mode: searchHeaderModeAll,
	})
	got := stderr.String()
	if !strings.Contains(got, "searching: all repos") {
		t.Errorf("fan-out header should say 'searching: all repos'; got %q", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("header must not write to stdout; got %q", stdout.String())
	}
}

func TestEmitSearchHeader_JSONModeSuppresses(t *testing.T) {
	for _, mode := range []searchHeaderMode{searchHeaderModeCwd, searchHeaderModeExplicit, searchHeaderModeAll} {
		var stderr, stdout bytes.Buffer
		emitSearchHeader(&stderr, &stdout, true, searchHeaderInfo{
			mode:    mode,
			shortID: "52d6c257dfe2",
			aliases: []string{"greetcli"},
		})
		if stderr.Len() != 0 {
			t.Errorf("mode=%v jsonOut=true should suppress stderr header; got %q", mode, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("mode=%v jsonOut=true must not write stdout header; got %q", mode, stdout.String())
		}
	}
}
