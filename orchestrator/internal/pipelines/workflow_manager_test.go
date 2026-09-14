package pipelines

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
)

// captureJobs relays every job sent to wf.jobs onto the returned channel so a
// test can assert on what RunWorkflowPipeline dispatched. Call stop() when done.
func captureJobs(wf *Workflow) (out chan Job, stop func()) {
	out = make(chan Job, 8)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case j := <-wf.jobs:
				out <- j
			case <-done:
				return
			}
		}
	}()
	return out, func() { close(done) }
}

func waitForJob(t *testing.T, ch chan Job, want string) {
	t.Helper()
	select {
	case job := <-ch:
		if job.GetJobType() != want {
			t.Errorf("JobType = %q, want %q", job.GetJobType(), want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %q job", want)
	}
}

func assertNoJob(t *testing.T, ch chan Job) {
	t.Helper()
	select {
	case job := <-ch:
		t.Fatalf("unexpected job dispatched: %+v", job)
	case <-time.After(100 * time.Millisecond):
	}
}

func newRunningPipeline(t *testing.T) (*WorkflowManager, chan *types.PullRequest, chan *types.AIEngineResponse) {
	t.Helper()
	wfm := NewWorkflowManager()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	prChan := make(chan *types.PullRequest)
	aierChan := make(chan *types.AIEngineResponse)
	pc := types.NewPushedCommits()

	// cli is nil: none of these test paths touch Docker.
	go wfm.RunWorkflowPipeline(ctx, nil, prChan, aierChan, pc)
	return wfm, prChan, aierChan
}

func TestRunWorkflowPipeline_AierDoneFalseDispatchesRunTests(t *testing.T) {
	wfm, _, aierChan := newRunningPipeline(t)

	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	jobs, stop := captureJobs(wf)
	t.Cleanup(stop)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	aierChan <- &types.AIEngineResponse{Wfid: 42, Done: false}
	waitForJob(t, jobs, "run_tests")
}

func TestRunWorkflowPipeline_AierDoneTrueDispatchesCommitPush(t *testing.T) {
	wfm, _, aierChan := newRunningPipeline(t)

	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	jobs, stop := captureJobs(wf)
	t.Cleanup(stop)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	aierChan <- &types.AIEngineResponse{Wfid: 42, Done: true}
	waitForJob(t, jobs, "commit_push")
}

func TestRunWorkflowPipeline_IgnoresUnknownWorkflowID(t *testing.T) {
	wfm, _, aierChan := newRunningPipeline(t)

	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	jobs, stop := captureJobs(wf)
	t.Cleanup(stop)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	aierChan <- &types.AIEngineResponse{Wfid: 999, Done: false}
	assertNoJob(t, jobs)
}

func TestRunWorkflowPipeline_IgnoresStoppedWorkflow(t *testing.T) {
	wfm, _, aierChan := newRunningPipeline(t)

	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	jobs, stop := captureJobs(wf)
	t.Cleanup(stop)
	close(wf.done) // mark not running
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	aierChan <- &types.AIEngineResponse{Wfid: 42, Done: false}
	assertNoJob(t, jobs)
}

func TestRunWorkflowPipeline_ForwardsPullRequestsToHandler(t *testing.T) {
	wfm, prChan, _ := newRunningPipeline(t)

	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	jobs, stop := captureJobs(wf)
	t.Cleanup(stop)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	prChan <- samplePRPtr("edited")
	waitForJob(t, jobs, "edit")
}

func TestRunWorkflowPipeline_ErrorCancelsAndRemovesWorkflow(t *testing.T) {
	wfm, _, _ := newRunningPipeline(t)

	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	cancelled := make(chan struct{})
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() { close(cancelled) }})

	wfm.wfErrChan <- ErrorObject{wfid: 42, err: errors.New("boom")}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for workflow cancellation")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := wfm.Get(42); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected workflow to be removed after error")
}
