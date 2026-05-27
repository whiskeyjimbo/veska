package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ErrInvalidURL is returned by CanonicalURL when raw cannot be parsed as a
// recognised git URL form (https, http, ssh://, git://, or scp-like
// [user@]host:path).
var ErrInvalidURL = errors.New("invalid git url")

// CanonicalURL returns the canonical form of a git URL used as the alias
// key for repo collision-resolution and as the input to DerivedRepoIDFromURL
// (solov2-kxo5.1).
//
// Rules:
//   - SSH scp-like form ([user@]host:path) is rewritten to https://host/path
//   - ssh:// and git:// schemes are rewritten to https://
//   - Host is lowercased; user-info is dropped
//   - Trailing .git on the path is stripped
//   - Trailing slash on the path is stripped
//   - Port (if present) is preserved
//   - Path case is preserved (some forges are case-sensitive)
//
// Anything that doesn't look like a URL at all returns ErrInvalidURL.
func CanonicalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalidURL
	}

	scheme, rest, ok := splitScheme(raw)
	if !ok {
		// scp-like: [user@]host:path. Must have a ':' separating host
		// from path, and the segment before ':' must not contain '/'.
		host, path, sep := strings.Cut(raw, ":")
		if !sep || host == "" || path == "" || strings.Contains(host, "/") {
			return "", ErrInvalidURL
		}
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if host == "" {
			return "", ErrInvalidURL
		}
		return normaliseURL("https", host, "/"+path), nil
	}

	switch scheme {
	case "https", "http", "ssh", "git":
		// fall through
	default:
		return "", ErrInvalidURL
	}

	// rest begins with the authority: [user@]host[:port][/path]
	authority, path, _ := strings.Cut(rest, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if authority == "" {
		return "", ErrInvalidURL
	}
	if path != "" {
		path = "/" + path
	}
	return normaliseURL("https", authority, path), nil
}

// splitScheme separates the leading scheme from raw and returns the
// remainder after "://". Returns ok=false if raw has no "scheme://" prefix.
func splitScheme(raw string) (scheme, rest string, ok bool) {
	idx := strings.Index(raw, "://")
	if idx <= 0 {
		return "", "", false
	}
	scheme = strings.ToLower(raw[:idx])
	for _, c := range scheme {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
			return "", "", false
		}
	}
	return scheme, raw[idx+3:], true
}

// normaliseURL applies the host-lowercase, .git-strip, trailing-slash-strip
// rules and assembles the canonical string. authority may include :port.
func normaliseURL(scheme, authority, path string) string {
	host, port, hasPort := strings.Cut(authority, ":")
	host = strings.ToLower(host)
	if hasPort {
		authority = host + ":" + port
	} else {
		authority = host
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	return scheme + "://" + authority + path
}

// Clone shells out to `git clone --depth=1 --progress url destDir`, streaming
// git's stderr (which carries --progress lines) to progressW so callers can
// render a live indicator. On failure the captured stderr is included
// verbatim in the returned error — never swallowed or paraphrased — so a
// permission/auth/404 diagnosis is obvious from one error string.
//
// destDir must be a path that does not yet exist (git clone refuses to
// clone into an existing non-empty directory). The returned path equals
// destDir on success.
func Clone(ctx context.Context, url, destDir string, progressW io.Writer) (string, error) {
	if progressW == nil {
		progressW = io.Discard
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", "--progress", url, destDir)
	var captured bytes.Buffer
	cmd.Stderr = io.MultiWriter(progressW, &captured)
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(captured.String())
		if stderr == "" {
			return "", fmt.Errorf("git clone %s: %w", url, err)
		}
		return "", fmt.Errorf("git clone %s: %w: %s", url, err, stderr)
	}
	return destDir, nil
}
