package configcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/service"
)

// repo mirrors the eng_list_repos row shape so decodeRepos can stage a fake
// response. Field tags match what RunReload decodes into.
type repo struct {
	RepoID  string `json:"repo_id"`
	ShortID string `json:"short_id"`
}

// decodeRepos populates RunReload's anonymous decode target by round-tripping
// through JSON — the same path mcpclient.Call uses — so the test stays
// independent of the unexported struct RunReload passes.
func decodeRepos(t *testing.T, outv any, repos []repo) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"repos": repos})
	if err != nil {
		t.Fatalf("marshal fake repos: %v", err)
	}
	if err := json.Unmarshal(b, outv); err != nil {
		t.Fatalf("decode into Call target: %v", err)
	}
}

// fakeManager is a service.Manager stub that records calls and lets each method
// be driven from the test.
type fakeManager struct {
	restartErr error
	statusErr  error
	restarts   int
}

func (f *fakeManager) Install(context.Context) error   { return nil }
func (f *fakeManager) Uninstall(context.Context) error { return nil }
func (f *fakeManager) Start(context.Context) error     { return nil }
func (f *fakeManager) Stop(context.Context) error      { return nil }
func (f *fakeManager) Restart(context.Context) error {
	f.restarts++
	return f.restartErr
}
func (f *fakeManager) Status(context.Context) (service.ServiceStatus, error) {
	return service.ServiceStatus{Running: true, PID: 1}, f.statusErr
}

// fastPoll returns ReloadParams poll knobs that keep tests sub-millisecond.
func fastPoll(p ReloadParams) ReloadParams {
	p.PollTimeout = 50 * time.Millisecond
	p.PollInterval = time.Millisecond
	return p
}

func TestRunReloadNilManager(t *testing.T) {
	err := RunReload(context.Background(), ReloadParams{Out: &bytes.Buffer{}})
	if !errors.Is(err, ErrNoManager) {
		t.Fatalf("want ErrNoManager, got %v", err)
	}
}

func TestRunReloadRestartFails(t *testing.T) {
	var out bytes.Buffer
	mgr := &fakeManager{restartErr: errors.New("boom")}
	err := RunReload(context.Background(), fastPoll(ReloadParams{
		Manager:     mgr,
		Out:         &out,
		DaemonReady: func() bool { return true },
		Call:        func(context.Context, string, any, any) error { return nil },
	}))
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("want restart error, got %v", err)
	}
}

func TestRunReloadDaemonNeverComesBack(t *testing.T) {
	var out bytes.Buffer
	err := RunReload(context.Background(), fastPoll(ReloadParams{
		Manager:     &fakeManager{},
		Out:         &out,
		DaemonReady: func() bool { return false },
		Call:        func(context.Context, string, any, any) error { return nil },
	}))
	if err == nil || !strings.Contains(err.Error(), "did not come back up") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestRunReloadNoRepos(t *testing.T) {
	var out bytes.Buffer
	err := RunReload(context.Background(), fastPoll(ReloadParams{
		Manager:     &fakeManager{},
		Out:         &out,
		DaemonReady: func() bool { return true },
		// eng_list_repos decodes into a struct with an empty Repos slice.
		Call: func(_ context.Context, method string, _ any, _ any) error { return nil },
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to re-scan") {
		t.Fatalf("want no-repos message, got %q", out.String())
	}
}

func TestRunReloadPromotesEachRepo(t *testing.T) {
	var out bytes.Buffer
	var promoted []string
	call := func(_ context.Context, method string, params, outv any) error {
		switch method {
		case "eng_list_repos":
			// Populate the caller's decode target via a typed assertion on the
			// anonymous struct pointer RunReload passes.
			decodeRepos(t, outv, []repo{{"r1", "alpha"}, {"r2", "beta"}})
			return nil
		case "eng_promote_repo":
			m := params.(map[string]any)
			promoted = append(promoted, m["repo_id"].(string))
			return nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil
		}
	}
	err := RunReload(context.Background(), fastPoll(ReloadParams{
		Manager: &fakeManager{}, Out: &out, DaemonReady: func() bool { return true }, Call: call,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(promoted) != 2 || promoted[0] != "r1" || promoted[1] != "r2" {
		t.Fatalf("want [r1 r2] promoted, got %v", promoted)
	}
	if !strings.Contains(out.String(), "2 repo(s) ok, 0 failed") {
		t.Fatalf("want success tally, got %q", out.String())
	}
}

func TestRunReloadPartialFailureReturnsError(t *testing.T) {
	var out bytes.Buffer
	call := func(_ context.Context, method string, params, outv any) error {
		switch method {
		case "eng_list_repos":
			decodeRepos(t, outv, []repo{{"r1", "alpha"}, {"r2", "beta"}})
			return nil
		case "eng_promote_repo":
			if params.(map[string]any)["repo_id"] == "r2" {
				return errors.New("promote failed")
			}
			return nil
		}
		return nil
	}
	err := RunReload(context.Background(), fastPoll(ReloadParams{
		Manager: &fakeManager{}, Out: &out, DaemonReady: func() bool { return true }, Call: call,
	}))
	if err == nil || !strings.Contains(err.Error(), "1 of 2 repos failed") {
		t.Fatalf("want partial-failure error, got %v", err)
	}
	if !strings.Contains(out.String(), "✗ beta") {
		t.Fatalf("want per-repo failure line, got %q", out.String())
	}
}
