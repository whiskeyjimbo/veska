package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// StatusProvider is an optional interface for eng_get_status.
// If nil, a static "ok" map is returned.
type StatusProvider interface {
	Status(ctx context.Context) (map[string]any, error)
}

// ConfigProvider is an optional interface for eng_get_config.
// If nil, a minimal static config is returned.
type ConfigProvider interface {
	Config(ctx context.Context) (map[string]any, error)
}

// RegisterAdminTools registers the 5 admin tools on r.
func RegisterAdminTools(
	r *Registry,
	repos application.RepoLister,
	status StatusProvider,
	cfg ConfigProvider,
) {
	r.MustRegister(ToolSpec{
		Name:            "eng_get_current_repo",
		Description:     "Find the indexed repo whose root contains the given cwd path.",
		IncludesStaging: true,
		InputSchema:     getCurrentRepoInputSchema,
		Handler:         makeGetCurrentRepoHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_list_repos",
		Description:     "List all indexed repos registered with the daemon.",
		IncludesStaging: false,
		InputSchema:     listReposInputSchema,
		Handler:         makeListReposHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_repo",
		Description:     "Get a single indexed repo by its repo_id.",
		IncludesStaging: false,
		InputSchema:     getRepoInputSchema,
		Handler:         makeGetRepoHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_status",
		Description:     "Return daemon liveness and schema version.",
		IncludesStaging: true,
		InputSchema:     getStatusInputSchema,
		Handler:         makeGetStatusHandler(status),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_config",
		Description:     "Return effective daemon configuration (secrets redacted).",
		IncludesStaging: false,
		InputSchema:     getConfigInputSchema,
		Handler:         makeGetConfigHandler(cfg),
	})
}

// eng_get_current_repo

type getCurrentRepoParams struct {
	CWD string `json:"cwd"`
}

func makeGetCurrentRepoHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getCurrentRepoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}

		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}

		// When cwd is omitted, fall back to the sole-registered-repo case
		// Editors and MCP clients can't easily inject their
		// project root automatically, and a junior tooling with exactly one
		// repo in flight gets the right answer without needing to know
		// about cwd at all. Multiple repos and no cwd is ambiguous — we
		// keep the loud invalid-params error so the caller learns it must
		// pass one.
		if p.CWD == "" {
			// Resolve to the sole *user-visible* repo. Synthetic ext:<module>
			// rows (from `veska deps index`) are hidden by eng_list_repos, so
			// counting them here would tell a caller "more than one repo" when
			// the listing shows exactly one, and would pick the wrong record;
			// skip them and default to the lone real repo.
			var sole *application.RepoRecord
			ambiguous := false
			for i := range all {
				if strings.HasPrefix(all[i].RepoID, "ext:") {
					continue
				}
				if sole != nil {
					ambiguous = true
					break
				}
				sole = &all[i]
			}
			if sole != nil && !ambiguous {
				return map[string]any{
					"repo":             decorateRepo(*sole),
					"included_staging": true,
					"degraded_reasons": []string{"defaulted_to_sole_repo"},
				}, nil
			}
			return nil, &RPCError{
				Code:    CodeInvalidParams,
				Message: `cwd is required when more than one repo is registered (pass {"cwd": "/abs/path/to/checkout"}; with a single registered repo, cwd may be omitted)`,
			}
		}

		for _, rec := range all {
			if strings.HasPrefix(p.CWD, rec.RootPath) {
				return map[string]any{
					"repo":             decorateRepo(rec),
					"included_staging": true,
					"degraded_reasons": []string{},
				}, nil
			}
		}

		return nil, &RPCError{Code: CodeInvalidParams, Message: "no indexed repo found for cwd"}
	}
}

// eng_list_repos
// RepoView decorates an application.RepoRecord with a derived 'status'
// field so callers (`veska repo list`, `doctor status`, AI tools) can
// distinguish a freshly-registered repo from a fully-indexed one
// without reverse-engineering empty strings.
// Status values:
//
//	"promoted" — last_promoted_sha is set; repo is queryable.
//	"unindexed" — last_promoted_sha is empty; repo was registered
//	  but the daemon has not (yet) cold-scanned it. Either the daemon
//	  is off, the daemon is mid-scan, or startup-resync errored on it
//	  ('s per-repo continue-on-error path).
//	"missing" — root_path no longer exists on disk; the registration
//	  is stale and queries against it will return nothing useful. CLI
//	  `veska repo list` has surfaced this for a while; MCP
//	  now matches so agents see the same signal.
type RepoView struct {
	RepoID          string `json:"repo_id"`
	ShortID         string `json:"short_id"`
	RootPath        string `json:"root_path"`
	ActiveBranch    string `json:"active_branch"`
	LastPromotedSHA string `json:"last_promoted_sha"`
	Status          string `json:"status"`
	// Kind is "tracked" (path-registered or `repo add <url>` clones) or
	// "ephemeral" (search --repo <url> cache-tier clones). Always set;
	// pre-kxo5.2 rows fall through the migration DEFAULT to 'tracked'
	Kind string `json:"kind"`
	// Aliases is the list of user-defined human-friendly names bound to
	// this repo. Empty when none are set. Accepted as a
	// repo_id substitute by every tool that resolves repo_id.
	Aliases []string `json:"aliases"`
}

// ShortRepoIDLen is the number of leading hex chars of a repo_id that the
// CLI and tools accept as a human-friendly alias. 12 chars of
// sha256 is collision-safe for any realistic number of tracked repos.
const ShortRepoIDLen = 12

// ShortRepoID returns the first ShortRepoIDLen chars of id, or id unchanged
// when it is already short.
func ShortRepoID(id string) string {
	if len(id) <= ShortRepoIDLen {
		return id
	}
	return id[:ShortRepoIDLen]
}

func decorateRepo(r application.RepoRecord) RepoView {
	status := "promoted"
	if r.LastPromotedSHA == "" {
		status = "unindexed"
	}
	// if the working-tree root no longer exists, report
	// "missing" so MCP callers see the same signal the CLI surfaces.
	if r.RootPath != "" {
		if _, err := os.Stat(r.RootPath); errors.Is(err, fs.ErrNotExist) {
			status = "missing"
		}
	}
	kind := r.Kind
	if kind == "" {
		// Defensive: an empty kind on the wire would render as "" in
		// `repo list`. Default to "tracked" so older callers (or any
		// row that bypassed the migration default) don't show a blank.
		kind = "tracked"
	}
	aliases := r.Aliases
	if aliases == nil {
		// README convention: empty result collections serialise as
		// rather than null. A nil string from the repo registry
		// would otherwise reach the wire as `"aliases": null` and
		// crash agents that iterate the field without a nil-check.
		aliases = []string{}
	}
	return RepoView{
		RepoID:          r.RepoID,
		ShortID:         ShortRepoID(r.RepoID),
		RootPath:        r.RootPath,
		ActiveBranch:    r.ActiveBranch,
		LastPromotedSHA: r.LastPromotedSHA,
		Status:          status,
		Kind:            kind,
		Aliases:         aliases,
	}
}

func decorateRepos(in []application.RepoRecord) []RepoView {
	out := make([]RepoView, 0, len(in))
	for _, r := range in {
		out = append(out, decorateRepo(r))
	}
	return out
}

// listReposParams accepts include_vendored=true to surface synthetic
// ext: repo rows alongside user-registered ones. Default false hides
// them so a multi-repo workspace's `eng_list_repos` result doesn't
// balloon with one entry per indexed dependency.
type listReposParams struct {
	IncludeVendored bool `json:"include_vendored"`
}

func makeListReposHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p listReposParams
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &p)
		}
		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}
		if !p.IncludeVendored {
			filtered := all[:0]
			for _, r := range all {
				if strings.HasPrefix(r.RepoID, "ext:") {
					continue
				}
				filtered = append(filtered, r)
			}
			all = filtered
		}

		return map[string]any{
			"repos":            decorateRepos(all),
			"included_staging": false,
			"degraded_reasons": []string{},
		}, nil
	}
}

// eng_get_repo

type getRepoParams struct {
	RepoID string `json:"repo_id"`
}

func makeGetRepoHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getRepoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}

		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}

		for _, rec := range all {
			if rec.RepoID == p.RepoID || ShortRepoID(rec.RepoID) == p.RepoID {
				return map[string]any{
					"repo":             decorateRepo(rec),
					"included_staging": false,
					"degraded_reasons": []string{},
				}, nil
			}
		}

		// not-found is a domain error, not a malformed-params error.
		return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
	}
}

// eng_get_status

func makeGetStatusHandler(sp StatusProvider) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if sp != nil {
			m, err := sp.Status(ctx)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("status failed: %v", err)}
			}
			return m, nil
		}

		return map[string]any{
			"status":           "ok",
			"schema_version":   1,
			"included_staging": true,
			"degraded_reasons": []string{},
		}, nil
	}
}

// eng_get_config

func makeGetConfigHandler(cp ConfigProvider) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if cp != nil {
			m, err := cp.Config(ctx)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("config failed: %v", err)}
			}
			return m, nil
		}

		return map[string]any{
			"veska_home":            config.DefaultVectorDir(),
			"config_schema_version": 1,
			"included_staging":      false,
			"degraded_reasons":      []string{},
		}, nil
	}
}
