package repo_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

func TestCanonicalURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https plain", "https://github.com/foo/bar", "https://github.com/foo/bar"},
		{"https .git suffix", "https://github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"https trailing slash", "https://github.com/foo/bar/", "https://github.com/foo/bar"},
		{"https .git then slash", "https://github.com/foo/bar.git/", "https://github.com/foo/bar"},
		{"https uppercase host", "https://GitHub.COM/foo/bar", "https://github.com/foo/bar"},
		{"https preserves path case", "https://github.com/Foo/Bar", "https://github.com/Foo/Bar"},
		{"https with port", "https://git.example.com:8443/team/repo", "https://git.example.com:8443/team/repo"},
		{"http scheme normalises to https", "http://github.com/foo/bar", "https://github.com/foo/bar"},
		{"git:// scheme", "git://github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"ssh:// scheme with user", "ssh://git@github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"scp-like git@ form", "git@github.com:foo/bar", "https://github.com/foo/bar"},
		{"scp-like with .git", "git@github.com:foo/bar.git", "https://github.com/foo/bar"},
		{"scp-like non-git user normalises", "user@gitlab.com:group/proj.git", "https://gitlab.com/group/proj"},
		{"surrounding whitespace trimmed", "  https://github.com/foo/bar  ", "https://github.com/foo/bar"},
		{"https url with userinfo drops user", "https://oauth2:tok@github.com/foo/bar", "https://github.com/foo/bar"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.CanonicalURL(tc.in)
			if err != nil {
				t.Fatalf("CanonicalURL(%q) returned err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("CanonicalURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalURL_Invalid(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"   ",
		"not-a-url",
		"://missing-scheme",
		"ftp://example.com/repo", // unsupported scheme
		"git@:nohost",            // no host before ':'
		"git@host:",              // no path after ':'
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := repo.CanonicalURL(in)
			if err == nil {
				t.Errorf("CanonicalURL(%q) want error, got nil", in)
			} else if !errors.Is(err, repo.ErrInvalidURL) {
				t.Errorf("CanonicalURL(%q) err = %v, want ErrInvalidURL", in, err)
			}
		})
	}
}

func TestCanonicalURL_SSHAndHTTPSAreEqual(t *testing.T) {
	t.Parallel()

	// The collision-resolution use case (kxo5.4 → kxo5.6) depends on these
	// producing identical canonical strings so DerivedRepoIDFromURL maps
	// both to the same row.
	pairs := [][2]string{
		{"git@github.com:foo/bar.git", "https://github.com/foo/bar"},
		{"ssh://git@github.com/foo/bar.git", "https://github.com/foo/bar/"},
		{"git@github.com:foo/bar", "https://GITHUB.com/foo/bar.git"},
	}
	for _, p := range pairs {
		a, errA := repo.CanonicalURL(p[0])
		b, errB := repo.CanonicalURL(p[1])
		if errA != nil || errB != nil {
			t.Fatalf("unexpected err: %v / %v", errA, errB)
		}
		if a != b {
			t.Errorf("equivalent inputs diverged:\n  %q → %q\n  %q → %q",
				p[0], a, p[1], b)
		}
	}
}

func TestClone_NoSuchLocalRepoSurfaceGitStderr(t *testing.T) {
	t.Parallel()

	// Use a definitely-nonexistent local path as the "url" so git exits
	// immediately with a clear error and no network is touched. This
	// covers AC3 (stderr verbatim) and AC's nil-progress-tolerance check
	// without depending on DNS or a remote host.
	missing := t.TempDir() + "/no-such-repo"
	_, err := repo.Clone(t.Context(), missing, t.TempDir()+"/dst", nil)
	if err == nil {
		t.Fatal("expected error for non-existent source")
	}
	if !strings.Contains(err.Error(), "git clone") {
		t.Errorf("err missing 'git clone' prefix: %v", err)
	}
	// Git's "does not exist" or "not a git repository" wording varies by
	// version; just check that some stderr context was attached.
	if !strings.Contains(err.Error(), missing) && !strings.Contains(err.Error(), "repository") {
		t.Errorf("err lacks captured stderr context: %v", err)
	}
}
