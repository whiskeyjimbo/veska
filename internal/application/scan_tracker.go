package application

import (
	"sort"
	"sync"
	"time"
)

// ScanState describes one cold scan currently in flight, surfaced to track
// indexing progress.
type ScanState struct {
	RepoID     string    `json:"repo_id"`
	Phase      string    `json:"phase"`
	StartedAt  time.Time `json:"started_at"`
	FilesSeen  int       `json:"files_seen,omitempty"`
	FilesTotal int       `json:"files_total,omitempty"`
}

// ScanTracker is the in-memory registry of cold-scan reparser runs currently
// in flight. Reads and writes are synchronized via an RWMutex.
type ScanTracker struct {
	mu    sync.RWMutex
	scans map[string]ScanState
}

func NewScanTracker() *ScanTracker {
	return &ScanTracker{scans: make(map[string]ScanState)}
}

// Start records that a scan for repoID has begun, overwriting older runs to
// capture the latest state.
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

// SetPhase updates the progress phase string (e.g. 'walking', 'promoting') so
// operators can distinguish slow promotion stages from fast walker scans.
func (t *ScanTracker) SetPhase(repoID, phase string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.scans[repoID]
	if !ok {
		return
	}
	st.Phase = phase
	t.scans[repoID] = st
}

func (t *ScanTracker) End(repoID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.scans, repoID)
}

// IsAnyScanRunning reports whether at least one scan is currently in flight.
func (t *ScanTracker) IsAnyScanRunning() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.scans) > 0
}

// Snapshot returns a copy of every in-flight scan, ordered by RepoID for
// stable serialization.
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
	sort.Slice(out, func(i, j int) bool { return out[i].RepoID < out[j].RepoID })
	return out
}
