#!/usr/bin/env bash
# install-bench-models.sh - download the model2vec variants used by the
# embed-models bench from Hugging Face into $VESKA_HOME/static-model/<name>/.
# Idempotent: skips models already on disk.
#
# Usage:
#   VESKA_HOME=$HOME/.veska tools/loadtest/embed_models/scripts/install-bench-models.sh
#
# Models pulled (~512MB total once installed):
#   potion-base-2M, potion-base-8M, potion-code-16M,
#   potion-retrieval-32M, potion-base-32M, potion-base-128M
#
# Requires: curl on PATH.

set -euo pipefail

: "${VESKA_HOME:=${HOME}/.veska}"

MODELS=(
    potion-base-2M
    potion-base-4M
    potion-base-8M
    potion-code-16M
    potion-retrieval-32M
    potion-base-32M
)

base="https://huggingface.co/minishlab"

for model in "${MODELS[@]}"; do
    dir="${VESKA_HOME}/static-model/${model}"
    mkdir -p "${dir}"

    if [[ -s "${dir}/tokenizer.json" && -s "${dir}/model.safetensors" ]]; then
        echo "install-bench-models: ${model} already installed - skip"
        continue
    fi

    echo "install-bench-models: pulling ${model}"
    curl -fsSL "${base}/${model}/resolve/main/tokenizer.json"    -o "${dir}/tokenizer.json"
    curl -fsSL "${base}/${model}/resolve/main/model.safetensors" -o "${dir}/model.safetensors"
done

echo "install-bench-models: done. installs at ${VESKA_HOME}/static-model/"
