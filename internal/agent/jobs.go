package agent

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/smokinstack/sddb/internal/types"
)

// errJobRunning is returned when a container already has a job in progress.
var errJobRunning = errors.New("a job is already running for this container")

const (
	jobTimeout   = 20 * time.Minute // upper bound for one upgrade or pull
	jobRetention = time.Hour        // how long finished jobs stay visible
)

// jobTracker runs long container actions (upgrade, pull) in the background,
// at most one per container name, and remembers their outcome.
//
// These used to run inside the HTTP request. When the dashboard's client gave
// up (90s for manual upgrades, 5 min for auto-update) the request context was
// cancelled, which killed `docker compose` mid-run and could leave a container
// removed but not started; auto-update then retried on top of it.
type jobTracker struct {
	mu   sync.Mutex
	jobs map[string]*types.JobStatus
	now  func() time.Time
}

func newJobTracker() *jobTracker {
	return &jobTracker{jobs: make(map[string]*types.JobStatus), now: time.Now}
}

// start launches fn in the background for the named container. It returns
// errJobRunning if a job for that container has not finished yet. done, if
// non-nil, runs after the job finishes (used to refresh stats).
func (t *jobTracker) start(name, action string, fn func(ctx context.Context) error, done func()) error {
	t.mu.Lock()
	if j, ok := t.jobs[name]; ok && j.State == types.JobRunning {
		t.mu.Unlock()
		return errJobRunning
	}
	job := &types.JobStatus{Action: action, State: types.JobRunning, Started: t.now().Unix()}
	t.jobs[name] = job
	t.mu.Unlock()

	go func() {
		// Deliberately not derived from the HTTP request: the job must outlive it.
		ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
		defer cancel()
		err := fn(ctx)

		t.mu.Lock()
		job.Finished = t.now().Unix()
		if err != nil {
			job.State = types.JobFailed
			job.Error = err.Error()
		} else {
			job.State = types.JobSucceeded
		}
		t.mu.Unlock()

		if err != nil {
			log.Printf("%s %s failed: %v", action, name, err)
		} else {
			log.Printf("%s %s succeeded", action, name)
		}
		if done != nil {
			done()
		}
	}()
	return nil
}

// snapshot returns a copy of current jobs and forgets finished ones older
// than jobRetention.
func (t *jobTracker) snapshot() map[string]types.JobStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.now().Add(-jobRetention).Unix()
	out := make(map[string]types.JobStatus, len(t.jobs))
	for name, j := range t.jobs {
		if j.State != types.JobRunning && j.Finished < cutoff {
			delete(t.jobs, name)
			continue
		}
		out[name] = *j
	}
	return out
}
