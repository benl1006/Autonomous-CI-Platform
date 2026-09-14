package pipelines

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
)

type stubGit struct {
	sha string
	err error
}

func (s stubGit) InitRepo(ctx context.Context, path string, pr types.PullRequest) error {
	return nil
}

func (s stubGit) AddAllCommitPush(commitMsg, wsPath, branch string) (string, error) {
	return s.sha, s.err
}

func samplePR(action string) types.PullRequest {
	return types.PullRequest{
		Number:  42,
		Action:  action,
		Branch:  "feat",
		HeadSHA: "abc",
	}
}

func samplePRPtr(action string) *types.PullRequest {
	pr := samplePR(action)
	return &pr
}

func TestWorkflowTrySendAndIsRunning(t *testing.T) {
	errCh := make(chan ErrorObject, 1)
	wf := newWorkflow(samplePRPtr("opened"), errCh)

	if !wf.isRunning() {
		t.Fatal("new workflow should be running")
	}

	received := make(chan Job, 1)
	go func() {
		received <- <-wf.jobs
	}()

	job, err := NewPullRequestJob("edit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !wf.trySend(job) {
		t.Fatal("trySend should deliver while running")
	}
	receivedJob := <-received
	if receivedJob.GetJobType() != "edit" {
		t.Errorf("job = %+v", receivedJob)
	}

	close(wf.done)
	if wf.isRunning() {
		t.Fatal("closed done channel should mark workflow stopped")
	}
	job, err = NewPullRequestJob("sync", nil)
	if err != nil {
		t.Fatal(err)
	}
	if wf.trySend(job) {
		t.Fatal("trySend should fail after workflow exits")
	}
}

func TestWorkflowPathAndCleanupAccessors(t *testing.T) {
	wf := newWorkflow(samplePRPtr("opened"), make(chan ErrorObject, 1))
	wf.SetPath("/tmp/ws")
	if wf.GetPath() != "/tmp/ws" {
		t.Errorf("path = %q", wf.GetPath())
	}

	called := false
	wf.SetCleanWorkspace(func() error {
		called = true
		return nil
	})
	fn := wf.GetCleanWorkspace()
	if fn == nil {
		t.Fatal("expected cleanup func")
	}
	if err := fn(); err != nil || !called {
		t.Fatalf("cleanup err=%v called=%v", err, called)
	}
}

func TestSendUpdatesToRemote(t *testing.T) {
	wf := newWorkflow(samplePRPtr("opened"), make(chan ErrorObject, 1))
	wf.workspace.path = "/ws"

	sha, err := wf.SendUpdatesToRemote(stubGit{sha: "newsha"})
	if err != nil {
		t.Fatal(err)
	}
	if sha != "newsha" {
		t.Errorf("sha = %q", sha)
	}

	_, err = wf.SendUpdatesToRemote(stubGit{err: errors.New("push failed")})
	if err == nil {
		t.Fatal("expected push error")
	}
}

func TestRunWorkflow_CancelDoesNotDoubleClose(t *testing.T) {
	prevTimeout := config.RequestCloseTimeout
	t.Cleanup(func() { config.RequestCloseTimeout = prevTimeout })
	config.RequestCloseTimeout = 0

	errCh := make(chan ErrorObject, 2)
	wf := newWorkflow(samplePRPtr("opened"), errCh)
	wf.workspace.removeWorkspace = func() error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		wf.runWorkflow(ctx, nil, types.NewPushedCommits())
		close(done)
	}()

	select {
	case <-done:
	case <-context.Background().Done():
	}

	select {
	case <-done:
	default:
		t.Fatal("runWorkflow did not return after cancel")
	}

	if wf.isRunning() {
		t.Fatal("workflow should not be running after exit")
	}
}

func TestWorkflowManagerGetSetRemove(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	got, ok := wfm.Get(42)
	if !ok || got.workflow != wf {
		t.Fatal("expected stored workflow")
	}
	wfm.Remove(42)
	if _, ok := wfm.Get(42); ok {
		t.Fatal("expected removal")
	}
}

func drainJobs(wf *Workflow) (stop func()) {
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-wf.jobs:
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func TestHandlePullRequest_EditedAndSynchronize(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	stop := drainJobs(wf)
	t.Cleanup(stop)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	if err := wfm.handlePullRequest(context.Background(), nil, samplePRPtr("edited"), types.NewPushedCommits()); err != nil {
		t.Fatalf("edited: %v", err)
	}
	if err := wfm.handlePullRequest(context.Background(), nil, samplePRPtr("synchronize"), types.NewPushedCommits()); err != nil {
		t.Fatalf("synchronize: %v", err)
	}
}

func TestHandlePullRequest_ClosedMergedRemovesWorkflow(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	cancelled := false
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() { cancelled = true }})

	pr := samplePR("closed")
	pr.Merged = true
	if err := wfm.handlePullRequest(context.Background(), nil, &pr, types.NewPushedCommits()); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("expected cancel")
	}
	if _, ok := wfm.Get(42); ok {
		t.Fatal("merged close should remove workflow")
	}
}

func TestHandlePullRequest_ClosedUnmergedKeepsWorkflow(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	if err := wfm.handlePullRequest(context.Background(), nil, samplePRPtr("closed"), types.NewPushedCommits()); err != nil {
		t.Fatal(err)
	}
	if _, ok := wfm.Get(42); !ok {
		t.Fatal("unmerged close should keep workflow")
	}
}

func TestHandlePullRequest_RejectsActionsOnStoppedWorkflow(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	close(wf.done)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	err := wfm.handlePullRequest(context.Background(), nil, samplePRPtr("edited"), types.NewPushedCommits())
	if err == nil {
		t.Fatal("expected error for edited on stopped workflow")
	}
}

func TestHandlePullRequest_RejectsReopenedWhileRunning(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	err := wfm.handlePullRequest(context.Background(), nil, samplePRPtr("reopened"), types.NewPushedCommits())
	if err == nil {
		t.Fatal("expected error for reopened while running")
	}
}

func TestHandlePullRequest_ReopenedResetsDoneChannel(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	close(wf.done)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	previousDone := wf.done
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := wfm.handlePullRequest(ctx, nil, samplePRPtr("reopened"), types.NewPushedCommits()); err != nil {
		t.Fatal(err)
	}
	if wf.done == previousDone {
		t.Fatal("reopened workflow should use a new done channel")
	}

	select {
	case <-time.After(time.Second):
		t.Fatal("reopened workflow did not stop")
	case <-wf.done:
	}
}

func TestHandlePullRequest_DuplicateOpenedPanics(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = wfm.handlePullRequest(context.Background(), nil, samplePRPtr("opened"), types.NewPushedCommits())
}

func TestHandlePullRequest_MissingWorkflowStartsWorkflow(t *testing.T) {
	wfm := NewWorkflowManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := wfm.handlePullRequest(ctx, nil, samplePRPtr("edited"), types.NewPushedCommits()); err != nil {
		t.Fatal(err)
	}
	if _, ok := wfm.Get(42); !ok {
		t.Fatal("expected missing workflow to be started")
	}
}

func TestHandlePullRequest_UnsupportedAction(t *testing.T) {
	wfm := NewWorkflowManager()
	wf := newWorkflow(samplePRPtr("opened"), wfm.wfErrChan)
	wfm.Set(42, WorkflowObject{workflow: wf, cancel: func() {}})
	if err := wfm.handlePullRequest(context.Background(), nil, samplePRPtr("assigned"), types.NewPushedCommits()); err != nil {
		t.Fatal(err)
	}
}
