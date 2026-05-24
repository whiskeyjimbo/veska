package application

import (
	"sync"
	"time"
)

// ScanState describes one cold scan currently in flight. Returned by
// ScanTracker.Snapshot so callers (e.g. statusProvider) can surface
// progress without coupling to the tracker's internals.
//
// FilesSeen / FilesTotal are best-effort progress counters updated by
// the scanner via Progress(). They are omitted from the JSON shape when
// zero so older clients see the same envelope (solov2-u9h9).
type ScanState struct {
	RepoID     string    `json:"repo_id"`
	Phase      string    `json:"phase"`
	StartedAt  time.Time `json:"started_at"`
	FilesSeen  int       `json:"files_seen,omitempty"`
	FilesTotal int       `json:"files_total,omitempty"`
}

// ScanTracker is the in-memory registry of cold-scan reparser runs
// currently in flight. The cold-scan closure calls Start at scan entry
// and End at scan exit; the daemon's statusProvider reads Snapshot to
// surface a 'scans_in_flight' field on eng_get_status (solov2-pm5).
//
// Concurrent safety: a single RWMutex guards the map. Reads (Snapshot
// from the status handler) take the read lock; writes (Start/End from
// the reparser goroutine) take the write lock. The scan-rate is low —
// at most one reparser per repo at a time — so the lock is
// uncontended in practice.
type ScanTracker struct {
	mu    sync.RWMutex
	scans map[string]ScanState
}

// NewScanTracker returns an empty tracker.
func NewScanTracker() *ScanTracker {
	return &ScanTracker{scans: make(map[string]ScanState)}
}

// Start records that a scan for repoID has begun. If a scan is already
// recorded for repoID (e.g. a concurrent eng_add_repo race), the new
// start replaces the old — the latest run is what's relevant to surface.
func (t *ScanTracker) Start(repoID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scans[repoID] = ScanState{
		RepoID:    repoID,
		Phase:     "running",
		StartedAt: time.Now(),
	}
}

// Progress updates the files_seen / files_total counters for an
// in-flight scan. Nil-safe and no-op if the scan was never Started
// (handles tracker-less unit tests).
func (t *ScanTracker) Progress(repoID string, filesSeen, filesTotal int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.scans[repoID]
	if !ok {
		return
	}
	st.FilesSeen = filesSeen
	st.FilesTotal = filesTotal
	t.scans[repoID] = st
}

// End removes the scan record for repoID. Idempotent — calling End for
// a repo that isn't tracked is a no-op (handles the failed-start path
// where reparser dispatch races with the start log).
func (t *ScanTracker) End(repoID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.scans, repoID)
}

// IsAnyScanRunning reports whether at least one scan is currently
// in flight. Cheaper than Snapshot for callers that only need a
// yes/no (e.g. the post-promotion queue gate in solov2-pc3 #1).
// Nil-safe — returns false when the tracker is unset.
func (t *ScanTracker) IsAnyScanRunning() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.scans) > 0
}

// Snapshot returns a copy of every in-flight scan, ordered by RepoID
// for stable consumption. Empty slice (never nil) when no scans are
// running.
func (t *ScanTracker) Snapshot() []ScanState {
	if t == nil {
		return []ScanState{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ScanState, 0, len(t.scans))
	for _, s := range t.scans {
		out = append(out, s)
	}
	// Stable order so the JSON shape doesn't churn between calls.
	sortScansByRepoID(out)
	return out
}

// sortScansByRepoID is split out so the lock-holding Snapshot stays
// short. A simple insertion-sort is fine here — the slice is at most
// 'number of registered repos' long, in practice tiny.
func sortScansByRepoID(s []ScanState) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].RepoID > s[j].RepoID; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
