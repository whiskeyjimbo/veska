package model2vec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// errDownloadFailed wraps every non-200 / transport-level failure
// from the model-download path so the daemon can decide (via
// errors.Is) whether to disable the model2vec branch and fall back
// to the hash-static embedder for this run.
var errDownloadFailed = errors.New("model2vec: download failed")

// FileSpec describes one file expected in the model directory along
// with the sha256 it must hash to. A cached file whose sha doesn't
// match is treated as corrupt and re-downloaded.
type FileSpec struct {
	Name   string
	SHA256 string // hex-encoded
}

// ModelSpec is the manifest the download path needs: where to fetch
// each file from (BaseURL + Name) and which sha to verify it against.
// Concrete model specs (potion-code-16M, etc.) live alongside the
// daemon composition root, not here — this package stays
// model-agnostic.
type ModelSpec struct {
	BaseURL string
	Files   []FileSpec
}

// Install downloads and sha-verifies the files in spec into
// <veskaHome>/static-model/<modelName>/ and returns that directory. It
// is idempotent: a file already present with a matching sha is left
// alone, so re-running is cheap. The concrete spec (HF base URL + per
// file shas) is supplied by the caller — this package stays
// model-agnostic. errDownloadFailed wraps transport/sha failures.
func Install(ctx context.Context, veskaHome, modelName string, spec ModelSpec) (string, error) {
	dir := ModelDir(veskaHome, modelName)
	if err := ensureModel(ctx, dir, spec); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureModel guarantees that every file in spec is present in dir
// and hashes to its declared sha256. Files already present with a
// matching sha are left alone. Files missing or wrong are
// re-downloaded into the same path. A partial download is removed on
// error so the next attempt re-tries cleanly.
func ensureModel(ctx context.Context, dir string, spec ModelSpec) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("model2vec: mkdir %s: %w", dir, err)
	}
	for _, f := range spec.Files {
		path := filepath.Join(dir, f.Name)
		if ok, _ := verifySHA(path, f.SHA256); ok {
			continue
		}
		if err := downloadFile(ctx, spec.BaseURL+"/"+f.Name, path); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("%w: %s: %v", errDownloadFailed, f.Name, err)
		}
		if ok, err := verifySHA(path, f.SHA256); !ok {
			_ = os.Remove(path)
			return fmt.Errorf("%w: %s sha mismatch: %v", errDownloadFailed, f.Name, err)
		}
	}
	return nil
}

// verifySHA returns (true, nil) when path exists and its sha256 matches
// want. A missing file returns (false, the stat error); a present but
// wrong file returns (false, nil) so the caller refetches.
func verifySHA(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return false, fmt.Errorf("sha got %s want %s", got, want)
	}
	return true, nil
}

// downloadFile streams a GET into path. The HTTP client uses the
// default 30s timeout via http.DefaultClient; callers wanting a
// custom timeout can wrap the parent context.
func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("stream %s: %w", path, err)
	}
	return nil
}
