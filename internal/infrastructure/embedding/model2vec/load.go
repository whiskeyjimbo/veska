package model2vec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrModelNotPresent is returned by TryLoad when the requested model
// directory does not yet contain tokenizer.json + model.safetensors.
// The daemon treats this as "skip model2vec, use hash-static" rather
// than a fatal error, so a fresh install boots without requiring the
// download to have completed.
var ErrModelNotPresent = errors.New("model2vec: model files not present")

// ModelDir returns the on-disk directory a named static model lives in:
// <veskaHome>/static-model/<modelName>/. Both TryLoad (read) and Install
// (write) resolve through here so the layout stays in one place.
func ModelDir(veskaHome, modelName string) string {
	return filepath.Join(veskaHome, "static-model", modelName)
}

// TryLoad attempts to build a Provider from <veskaHome>/static-model/
// <modelName>/. Returns ErrModelNotPresent when either required file
// is missing — callers can errors.Is on it to decide whether to fall
// back. Any other error (corrupt file, unsupported tokenizer, etc.)
// surfaces normally so a broken install isn't silently masked.
func TryLoad(veskaHome, modelName string) (*Provider, error) {
	dir := ModelDir(veskaHome, modelName)
	for _, name := range []string{"tokenizer.json", "model.safetensors"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: %s", ErrModelNotPresent, filepath.Join(dir, name))
			}
			return nil, fmt.Errorf("model2vec: stat %s: %w", name, err)
		}
	}
	return New(dir)
}
