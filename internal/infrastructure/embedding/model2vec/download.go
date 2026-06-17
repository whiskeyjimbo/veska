// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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

// errDownloadFailed represents a transport or checksum verification failure during embedder model download.
var errDownloadFailed = errors.New("model2vec: download failed")

// FileSpec defines the expected filename and SHA-256 checksum for verification.
type FileSpec struct {
	Name   string
	SHA256 string // hex-encoded
}

// ModelSpec defines the download location and expected file signatures for an embedder model.
type ModelSpec struct {
	BaseURL string
	Files   []FileSpec
}

// Install downloads and verifies model files into the target directories, skipping files that already match their checksums.
func Install(ctx context.Context, veskaHome, modelName string, spec ModelSpec) (string, error) {
	dir := ModelDir(veskaHome, modelName)
	if err := ensureModel(ctx, dir, spec); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureModel downloads and verifies all files defined in the model specification.
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

// verifySHA checks if the file at path exists and matches the expected SHA-256 checksum.
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

// downloadFile streams an HTTP GET request to the specified path.
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
