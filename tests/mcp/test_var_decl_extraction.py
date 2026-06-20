"""Tests for top-level Go var/const extraction .

Pre- the tree-sitter Go extractor emitted only function/method/type
nodes; package-scope var declarations were invisible. That made cobra
CLIs (and any other framework where the API surface lives in initialised
vars) look empty to eng_find_symbol. These tests pin the new behaviour
end-to-end through the MCP socket: register a tiny cobra-style repo,
wait for cold scan + promotion, then assert the rootCmd / multi-var
declarations come back as kind='variable' nodes."""

from __future__ import annotations

import os
import subprocess
import tempfile
import time


COBRA_FIXTURE = """package main

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{
\tUse:   "tool",
\tShort: "demo cli",
}

var (
\tverbose bool
\tlogFile string
)

const buildMode = "release"

func main() { rootCmd.Execute() }
"""


def _init_cobra_repo(tmp: str) -> None:
    subprocess.run(["git", "-C", tmp, "init", "-q", "-b", "main"], check=True)
    subprocess.run(["git", "-C", tmp, "config", "user.email", "harness@example.invalid"], check=True)
    subprocess.run(["git", "-C", tmp, "config", "user.name", "harness"], check=True)
    with open(os.path.join(tmp, "main.go"), "w") as f:
        f.write(COBRA_FIXTURE)
    subprocess.run(["git", "-C", tmp, "add", "-A"], check=True)
    subprocess.run(["git", "-C", tmp, "commit", "-q", "-m", "init"], check=True)


def _wait_for_promotion(mcp_client, repo_id: str, timeout_s: float = 15.0) -> bool:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        _, _, _, result = mcp_client.call("eng_get_repo", {"repo_id": repo_id})
        rec = result.get("repo") if isinstance(result, dict) else None
        if rec and rec.get("last_promoted_sha"):
            return True
        time.sleep(0.5)
    return False


def test_top_level_var_declarations_become_variable_nodes(mcp_client):
    """plain top-level var-block names (verbose, logFile) and
    the const buildMode surface as kind='variable' nodes via eng_find_symbol.

    rootCmd is the exception: `var rootCmd = &cobra.Command{...}` is no
    longer a plain variable - the cobra extractor ('promote cobra
    command literals to KindCommand') reclassifies it as a kind='command'
    node keyed by its Use string ("tool"), NOT by the Go var name. So we
    assert the command surfaces under "tool" rather than expecting a
    'rootCmd' variable (re-pinned after the extractor change)."""
    with tempfile.TemporaryDirectory(prefix="veska-mcp-cobra-") as tmp:
        _init_cobra_repo(tmp)

        ok, text, _, add_result = mcp_client.call("eng_add_repo", {"root_path": tmp})
        assert ok, f"eng_add_repo failed: {text}"
        repo_id = add_result["repo_id"]
        try:
            assert _wait_for_promotion(mcp_client, repo_id), (
                "fixture repo never reached promoted state - cold scan stuck?"
            )
            # Plain var/const declarations → kind='variable'.
            for name in ("verbose", "logFile", "buildMode"):
                ok, text, _, res = mcp_client.call("eng_find_symbol", {
                    "repo_id": repo_id, "symbol": name,
                })
                assert ok, f"eng_find_symbol({name!r}) failed: {text}"
                nodes = res.get("nodes") or []
                assert nodes, f"expected {name!r} extracted as a variable node, got nothing"
                kinds = {n.get("kind") for n in nodes}
                assert "variable" in kinds, (
                    f"{name!r}: expected at least one kind='variable' node, got kinds={kinds}"
                )

            # rootCmd is promoted to a cobra command keyed by its Use string.
            ok, text, _, res = mcp_client.call("eng_find_symbol", {
                "repo_id": repo_id, "symbol": "tool",
            })
            assert ok, f"eng_find_symbol('tool') failed: {text}"
            nodes = res.get("nodes") or []
            kinds = {n.get("kind") for n in nodes}
            assert "command" in kinds, (
                f"expected the cobra rootCmd as a kind='command' node, got kinds={kinds}"
            )
        finally:
            mcp_client.call("eng_remove_repo", {"repo_id": repo_id})
