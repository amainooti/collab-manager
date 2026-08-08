// Package jobs tracks background work (research/draft calls, which can run
// for a couple of minutes against an LLM) so the web UI can poll and show
// progress instead of blocking a request the whole time — the problem this
// solves: a plain synchronous form POST gives no feedback while it's
// pending, so a user who navigates away has no way to tell it's still
// running until they come back and the page has already changed.
//
// State is in-memory and keyed by an arbitrary string (the web package uses
// "research:<projectID>" / "draft:<projectID>") — it doesn't need to
// survive a restart, it just needs to answer "is this still running" while
// the process is up.
package jobs

import "sync"

type Status string

const (
	StatusIdle    Status = "idle" // never run, or result already consumed
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusError   Status = "error"
)

type State struct {
	Status  Status
	Message string // human-readable "what's happening", shown under the progress bar
	Err     string // set when Status == StatusError
}

type Tracker struct {
	mu    sync.Mutex
	state map[string]State
}

func NewTracker() *Tracker {
	return &Tracker{state: map[string]State{}}
}

// Start marks a job running. Returns false without changing anything if a
// job with this key is already running — callers use this to avoid
// double-firing an LLM call if the user double-clicks or two tabs poll in.
func (t *Tracker) Start(key, message string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state[key].Status == StatusRunning {
		return false
	}
	t.state[key] = State{Status: StatusRunning, Message: message}
	return true
}

func (t *Tracker) Finish(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state[key] = State{Status: StatusDone}
}

func (t *Tracker) Fail(key string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state[key] = State{Status: StatusError, Err: err.Error()}
}

// Clear resets a key to idle — used once a Done/Error result has been
// rendered, so the next poll or page load sees a clean slate rather than
// re-showing a stale terminal state forever.
func (t *Tracker) Clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.state, key)
}

func (t *Tracker) Get(key string) State {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.state[key]; ok {
		return s
	}
	return State{Status: StatusIdle}
}
