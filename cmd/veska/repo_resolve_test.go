package main

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/repo"
)

func TestResolveCLIRepoID_ErrorWording(t *testing.T) {
	recs := []repo.Record{
		{RepoID: "abcd1234ef567890aaaabbbbccccddddeeeeffff0000111122223333aaaabbbb", Aliases: []string{"greetcli"}},
	}
	cases := []struct {
		name       string
		input      string
		wantSubstr string
	}{
		{
			name:       "short input mentions prefix length floor",
			input:      "xy",
			wantSubstr: "prefixes must be >=",
		},
		{
			name:       "long input no longer falsely cites the prefix floor (solov2-fdni)",
			input:      "greetlib",
			wantSubstr: "no match by full id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveCLIRepoID(recs, tc.input)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestResolveCLIRepoID_MatchesByAlias(t *testing.T) {
	recs := []repo.Record{
		{RepoID: "abcd1234ef567890", Aliases: []string{"greetcli"}},
	}
	got, err := resolveCLIRepoID(recs, "greetcli")
	if err != nil {
		t.Fatalf("alias lookup failed: %v", err)
	}
	if got.RepoID != "abcd1234ef567890" {
		t.Errorf("got %q", got.RepoID)
	}
}
