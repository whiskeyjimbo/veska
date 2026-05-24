package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// injectCwdAndCopy reads newline-delimited JSON-RPC frames from src and
// writes them to dst, injecting the shim's working directory into
// eng_get_current_repo requests that don't already carry a cwd param
// (solov2-k8zc). Non-JSON or non-target frames pass through byte-for-byte.
//
// The shim is normally a pure byte pump, but tools that key off the
// caller's filesystem location need cwd, and most MCP clients (Claude
// Desktop, Cursor, raw `printf | veska-mcp`) have no way to express
// that. The shim already runs in the user's working directory — using
// it as a fallback Just Works for the common case.
func injectCwdAndCopy(dst io.Writer, src io.Reader) {
	cwd, _ := os.Getwd()
	if cwd == "" {
		// Without a cwd to inject, the rewrite is a no-op; fall back to a
		// straight copy so we don't pay the per-frame parse cost.
		_, _ = io.Copy(dst, src)
		return
	}
	r := bufio.NewReader(src)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			rewritten, ok := maybeInjectCwd(line, cwd)
			out := line
			if ok {
				out = rewritten
			}
			if _, werr := dst.Write(out); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// maybeInjectCwd parses a single newline-delimited JSON-RPC frame and
// returns a rewritten version if it's an eng_get_current_repo call that
// omits cwd, plus a flag indicating whether a rewrite happened. Non-JSON
// frames or other methods return (nil, false) so the caller passes the
// original bytes through unchanged.
func maybeInjectCwd(line []byte, cwd string) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var msg map[string]any
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return nil, false
	}
	method, _ := msg["method"].(string)
	if method != "eng_get_current_repo" {
		return nil, false
	}
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
		msg["params"] = params
	}
	if existing, ok := params["cwd"].(string); ok && existing != "" {
		return nil, false
	}
	params["cwd"] = cwd
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return append(out, '\n'), true
}
