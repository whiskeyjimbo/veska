// SPDX-License-Identifier: AGPL-3.0-only

package model2vec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrModelNotPresent is returned when the model directory lacks required model weights or tokenizer definitions.
var ErrModelNotPresent = errors.New("model2vec: model files not present")

// ModelDir returns the filesystem path where model assets are stored.
func ModelDir(veskaHome, modelName string) string {
	return filepath.Join(veskaHome, "static-model", modelName)
}

// TryLoad loads the Model2Vec provider from the specified Veska home directory.
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
