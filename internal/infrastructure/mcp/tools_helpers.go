package mcp

import (
	"encoding/json"
	"fmt"
)

// bindParams unmarshals raw into dst, returning an InvalidParams RPCError on failure.
func bindParams(raw json.RawMessage, dst any) *RPCError {
	if err := json.Unmarshal(raw, dst); err != nil {
		return &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
	}
	return nil
}

// checkRequired returns an InvalidParams RPCError for the first empty value in
// the alternating name/value pairs. E.g.: checkRequired("repo_id", p.RepoID, "branch", p.Branch).
func checkRequired(nameVal ...string) *RPCError {
	for i := 0; i+1 < len(nameVal); i += 2 {
		if nameVal[i+1] == "" {
			return &RPCError{Code: CodeInvalidParams, Message: nameVal[i] + " is required"}
		}
	}
	return nil
}
