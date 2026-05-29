package repo_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

func TestSetAlias_Insert(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"abc", "/tmp/abc", 1,
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAlias(context.Background(), db, "foo", "abc", false); err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	got, ok, err := repo.LookupAlias(context.Background(), db, "foo")
	if err != nil || !ok || got != "abc" {
		t.Fatalf("LookupAlias = (%q,%v,%v); want (abc,true,nil)", got, ok, err)
	}
}

func TestSetAlias_ConflictRequiresForce(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"a", "b"} {
		if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
			id, "/tmp/"+id, 1,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SetAlias(context.Background(), db, "x", "a", false); err != nil {
		t.Fatal(err)
	}
	err := repo.SetAlias(context.Background(), db, "x", "b", false)
	if !errors.Is(err, repo.ErrAliasExists) {
		t.Fatalf("expected ErrAliasExists, got %v", err)
	}
	if err := repo.SetAlias(context.Background(), db, "x", "b", true); err != nil {
		t.Fatalf("force: %v", err)
	}
	got, _, _ := repo.LookupAlias(context.Background(), db, "x")
	if got != "b" {
		t.Errorf("after force, lookup = %q; want b", got)
	}
}

func TestSetAlias_SameTargetIsNoop(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"abc", "/tmp/abc", 1,
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAlias(context.Background(), db, "x", "abc", false); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAlias(context.Background(), db, "x", "abc", false); err != nil {
		t.Errorf("re-binding to same repo should be no-op; got %v", err)
	}
}

func TestRemoveAlias_NotFound(t *testing.T) {
	db := newTestDB(t)
	err := repo.RemoveAlias(context.Background(), db, "nope")
	if !errors.Is(err, repo.ErrAliasNotFound) {
		t.Fatalf("expected ErrAliasNotFound, got %v", err)
	}
}

func TestValidateAliasName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want error
	}{
		{"", repo.ErrAliasInvalid},
		{"has space", repo.ErrAliasInvalid},
		{"deadbeef", repo.ErrAliasInvalid}, // hex-only >= 4 chars
		{"abcd", repo.ErrAliasInvalid},
		{"my-repo", nil},
		{"foo", nil}, // 3 chars, not hex-prefix-length
		{"abc", nil}, // hex but < 4 chars
	}
	for _, c := range cases {
		err := repo.ValidateAliasName(c.name)
		if c.want == nil && err != nil {
			t.Errorf("ValidateAliasName(%q) = %v; want nil", c.name, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("ValidateAliasName(%q) = %v; want %v", c.name, err, c.want)
		}
	}
}

func TestSetAlias_RejectsInvalidName(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"abc", "/tmp/abc", 1,
	); err != nil {
		t.Fatal(err)
	}
	err := repo.SetAlias(context.Background(), db, "deadbeef", "abc", false)
	if !errors.Is(err, repo.ErrAliasInvalid) {
		t.Errorf("expected ErrAliasInvalid for hex-only name; got %v", err)
	}
}

func TestSuggestAliasNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		canonicalURL, rootPath string
		wantPrimary            string
		wantFallback           string
	}{
		{"https://github.com/foo/bar", "", "bar", "foo-bar"},
		{"https://github.com/foo/bar.git", "", "bar", "foo-bar"},
		{"https://example.com/single", "", "single", ""}, // only one path segment, no fallback
		{"", "/home/jrose/src/myproj", "myproj", ""},
		{"", "", "", ""},
	}
	for _, c := range cases {
		p, f := repo.SuggestAliasNames(c.canonicalURL, c.rootPath)
		if p != c.wantPrimary || f != c.wantFallback {
			t.Errorf("SuggestAliasNames(%q,%q) = (%q,%q); want (%q,%q)",
				c.canonicalURL, c.rootPath, p, f, c.wantPrimary, c.wantFallback)
		}
	}
}

func TestAliasesByRepoID(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"a", "b"} {
		if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
			id, "/tmp/"+id, 1,
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"one", "two"} {
		if err := repo.SetAlias(context.Background(), db, name, "a", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SetAlias(context.Background(), db, "three", "b", false); err != nil {
		t.Fatal(err)
	}

	got, err := repo.AliasesByRepoID(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"a": {"one", "two"},
		"b": {"three"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AliasesByRepoID = %+v; want %+v", got, want)
	}
}

func TestList_PopulatesAliases(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"abc", "/tmp/abc", 1,
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAlias(context.Background(), db, "myrepo", "abc", false); err != nil {
		t.Fatal(err)
	}
	recs, err := repo.List(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	if !reflect.DeepEqual(recs[0].Aliases, []string{"myrepo"}) {
		t.Errorf("rec.Aliases = %v; want [myrepo]", recs[0].Aliases)
	}
}

func TestCascadeDelete_RemovesAliases(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"abc", "/tmp/abc", 1,
	); err != nil {
		t.Fatal(err)
	}
	// Enable FK enforcement (off by default).
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAlias(context.Background(), db, "x", "abc", false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM repos WHERE repo_id = ?`, "abc"); err != nil {
		t.Fatal(err)
	}
	_, ok, err := repo.LookupAlias(context.Background(), db, "x")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("alias should have been dropped via FK CASCADE")
	}
}
