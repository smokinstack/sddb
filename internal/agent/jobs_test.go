package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/smokinstack/sddb/internal/types"
)

func TestJobTrackerOneJobPerContainer(t *testing.T) {
	tr := newJobTracker()
	release := make(chan struct{})
	done := make(chan struct{})

	err := tr.start("web", "upgrade", func(ctx context.Context) error { <-release; return nil }, func() { close(done) })
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.start("web", "pull", func(context.Context) error { return nil }, nil); !errors.Is(err, errJobRunning) {
		t.Fatalf("second job for same container: err = %v, want errJobRunning", err)
	}
	if got := tr.snapshot()["web"]; got.State != types.JobRunning || got.Action != "upgrade" {
		t.Fatalf("snapshot = %+v, want running upgrade", got)
	}

	close(release)
	<-done
	if got := tr.snapshot()["web"]; got.State != types.JobSucceeded || got.Finished == 0 {
		t.Fatalf("snapshot = %+v, want succeeded", got)
	}
	// Finished jobs don't block new ones.
	if err := tr.start("web", "pull", func(context.Context) error { return nil }, nil); err != nil {
		t.Fatalf("new job after completion: %v", err)
	}
}

func TestJobTrackerRecordsFailureAndExpires(t *testing.T) {
	tr := newJobTracker()
	now := time.Unix(1_000_000, 0)
	tr.now = func() time.Time { return now }
	done := make(chan struct{})

	tr.start("db", "upgrade", func(context.Context) error { return errors.New("pull failed") }, func() { close(done) })
	<-done

	got := tr.snapshot()["db"]
	if got.State != types.JobFailed || got.Error != "pull failed" {
		t.Fatalf("snapshot = %+v, want failed with error", got)
	}
	now = now.Add(jobRetention + time.Minute)
	if _, ok := tr.snapshot()["db"]; ok {
		t.Fatal("finished job should be forgotten after the retention period")
	}
}

// An upgrade/pull request must return immediately and keep running after
// the HTTP request (and its context) is gone.
func TestCommandStartsJobInBackground(t *testing.T) {
	pr, pw := io.Pipe()
	f := newFake()
	f.inspect = runningContainer()
	f.pullReader = pr // blocks until we write/close

	a := New(Config{}, &DockerClient{api: f})
	jobDone := make(chan struct{})
	a.onJobDone = func() { close(jobDone) }

	reqCtx, cancelReq := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/containers/"+oldID+"/pull", nil).WithContext(reqCtx)
	rec := httptest.NewRecorder()

	start := time.Now()
	a.handleCommand(rec, req)
	if time.Since(start) > time.Second {
		t.Fatal("handler blocked on the pull instead of returning immediately")
	}
	var resp types.CommandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || !resp.Pending {
		t.Fatalf("response = %+v, want success+pending", resp)
	}

	// The dashboard giving up must not cancel the job.
	cancelReq()

	// A second request while it runs is refused, not stacked.
	rec2 := httptest.NewRecorder()
	a.handleCommand(rec2, httptest.NewRequest(http.MethodPost, "/api/containers/"+oldID+"/upgrade", nil))
	var resp2 types.CommandResponse
	json.NewDecoder(rec2.Body).Decode(&resp2)
	if resp2.Success {
		t.Fatalf("concurrent job accepted: %+v", resp2)
	}

	pw.Write([]byte(`{"status":"Downloaded newer image"}`))
	pw.Close()
	select {
	case <-jobDone:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	if got := a.jobs.snapshot()["app"]; got.State != types.JobSucceeded || got.Action != "pull" {
		t.Fatalf("job = %+v, want succeeded pull", got)
	}
}
