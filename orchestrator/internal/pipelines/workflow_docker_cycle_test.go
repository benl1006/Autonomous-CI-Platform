package pipelines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
)

// scriptedDockerClient is a fake DockerClient that lets a test script exit
// codes across successive containers, so a single test can drive several
// run_tests cycles without a real Docker daemon.
type scriptedDockerClient struct {
	mu              sync.Mutex
	exitCodes       []int
	callIdx         int
	currentExitCode int
	inspectCalls    int
}

func (f *scriptedDockerClient) ImageList(ctx context.Context, options dockerClient.ImageListOptions) (dockerClient.ImageListResult, error) {
	return dockerClient.ImageListResult{}, nil
}

func (f *scriptedDockerClient) ImageRemove(ctx context.Context, tag string, options dockerClient.ImageRemoveOptions) (dockerClient.ImageRemoveResult, error) {
	return dockerClient.ImageRemoveResult{}, nil
}

func (f *scriptedDockerClient) ImageBuild(ctx context.Context, buildContext io.Reader, options dockerClient.ImageBuildOptions) (dockerClient.ImageBuildResult, error) {
	_, _ = io.Copy(io.Discard, buildContext) // drain the tar stream like a real daemon would
	return dockerClient.ImageBuildResult{Body: io.NopCloser(strings.NewReader(`{"stream":"built"}`))}, nil
}

func (f *scriptedDockerClient) ContainerList(ctx context.Context, options dockerClient.ContainerListOptions) (dockerClient.ContainerListResult, error) {
	return dockerClient.ContainerListResult{}, nil
}

func (f *scriptedDockerClient) ContainerCreate(ctx context.Context, options dockerClient.ContainerCreateOptions) (dockerClient.ContainerCreateResult, error) {
	return dockerClient.ContainerCreateResult{ID: "fake-container"}, nil
}

func (f *scriptedDockerClient) ContainerRemove(ctx context.Context, containerID string, options dockerClient.ContainerRemoveOptions) (dockerClient.ContainerRemoveResult, error) {
	return dockerClient.ContainerRemoveResult{}, nil
}

func (f *scriptedDockerClient) ContainerLogs(ctx context.Context, containerID string, options dockerClient.ContainerLogsOptions) (dockerClient.ContainerLogsResult, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *scriptedDockerClient) ContainerStart(ctx context.Context, containerID string, options dockerClient.ContainerStartOptions) (dockerClient.ContainerStartResult, error) {
	return dockerClient.ContainerStartResult{}, nil
}

func (f *scriptedDockerClient) ContainerWait(ctx context.Context, containerID string, options dockerClient.ContainerWaitOptions) dockerClient.ContainerWaitResult {
	resCh := make(chan container.WaitResponse, 1)
	resCh <- container.WaitResponse{}
	return dockerClient.ContainerWaitResult{Result: resCh, Error: make(chan error)}
}

func (f *scriptedDockerClient) ContainerInspect(ctx context.Context, containerID string, options dockerClient.ContainerInspectOptions) (dockerClient.ContainerInspectResult, error) {
	f.mu.Lock()
	if f.inspectCalls%2 == 0 {
		idx := f.callIdx
		if idx >= len(f.exitCodes) {
			idx = len(f.exitCodes) - 1
		}
		f.currentExitCode = f.exitCodes[idx]
		f.callIdx++
	}
	exitCode := f.currentExitCode
	f.inspectCalls++
	f.mu.Unlock()

	start := time.Now()
	end := start.Add(time.Millisecond)
	return dockerClient.ContainerInspectResult{
		Container: container.InspectResponse{
			State: &container.State{
				ExitCode:   exitCode,
				StartedAt:  start.Format(time.RFC3339Nano),
				FinishedAt: end.Format(time.RFC3339Nano),
				Status:     container.StateExited,
			},
		},
	}, nil
}

type recordedRequest struct {
	jobType string
	req     types.AIEngineRequest
}

func newFakeAIEngine(t *testing.T) (*httptest.Server, chan recordedRequest) {
	t.Helper()
	received := make(chan recordedRequest, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req types.AIEngineRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		received <- recordedRequest{jobType: r.Header.Get("Job-Type"), req: req}
	}))
	t.Cleanup(srv.Close)
	return srv, received
}

func TestRunWorkflow_MultipleRunTestsCyclesThenClose(t *testing.T) {
	prevURL, prevSecret, prevCloseTimeout := config.AIEngineURL, config.InternalSecret, config.RequestCloseTimeout
	t.Cleanup(func() {
		config.AIEngineURL = prevURL
		config.InternalSecret = prevSecret
		config.RequestCloseTimeout = prevCloseTimeout
	})
	config.InternalSecret = "testsecret"
	config.RequestCloseTimeout = 2

	srv, received := newFakeAIEngine(t)
	config.AIEngineURL = srv.URL

	pr := samplePR("opened")
	errCh := make(chan ErrorObject, 8)
	wf := newWorkflow(&pr, errCh)
	wf.workspace.path = t.TempDir()
	wf.workspace.removeWorkspace = func() error { return nil }

	fakeCli := &scriptedDockerClient{exitCodes: []int{1, 1, 0}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		wf.runWorkflow(ctx, fakeCli, types.NewPushedCommits())
		close(done)
	}()

	for i, wantExit := range []int{1, 1, 0} {
		job, err := NewAIEJob("run_tests", &types.AIEngineResponse{
			PullRequest: pr,
			TestCmd:     []string{"pytest", fmt.Sprintf("cycle_%d_test.go", i)},
			TestName:    fmt.Sprintf("cycle_%d_test.go", i),
			Tests:       []byte("package cycle"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !wf.trySend(job) {
			t.Fatalf("cycle %d: workflow stopped before accepting job", i)
		}

		select {
		case rec := <-received:
			if rec.jobType != "logs" {
				t.Fatalf("cycle %d: Job-Type = %q, want %q", i, rec.jobType, "logs")
			}
			if rec.req.ExitCode != wantExit {
				t.Errorf("cycle %d: ExitCode = %d, want %d", i, rec.req.ExitCode, wantExit)
			}
			if rec.req.Wfid != wf.wfid {
				t.Errorf("cycle %d: Wfid = %d, want %d", i, rec.req.Wfid, wf.wfid)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("cycle %d: timed out waiting for logs callback", i)
		}
	}

	if wf.attemptNum != 3 {
		t.Errorf("attemptNum = %d, want 3", wf.attemptNum)
	}

	cancel()
	select {
	case rec := <-received:
		if rec.jobType != "close" {
			t.Errorf("Job-Type = %q, want %q", rec.jobType, "close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for close callback")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWorkflow did not exit after cancel")
	}

	if wf.isRunning() {
		t.Fatal("workflow should not be running after close")
	}

	select {
	case errObj := <-errCh:
		t.Fatalf("unexpected error on error channel (wfid=%d): %v", errObj.wfid, errObj.err)
	default:
	}
}

func TestRunWorkflow_StopsAfterMaxTestPatchingAttempts(t *testing.T) {
	prevURL, prevSecret, prevMax := config.AIEngineURL, config.InternalSecret, config.MaxTestPatchingAttempts
	t.Cleanup(func() {
		config.AIEngineURL = prevURL
		config.InternalSecret = prevSecret
		config.MaxTestPatchingAttempts = prevMax
	})
	config.InternalSecret = "testsecret"
	config.MaxTestPatchingAttempts = 2

	srv, received := newFakeAIEngine(t)
	config.AIEngineURL = srv.URL

	pr := samplePR("opened")
	errCh := make(chan ErrorObject, 8)
	wf := newWorkflow(&pr, errCh)
	wf.workspace.path = t.TempDir()
	wf.workspace.removeWorkspace = func() error { return nil }

	fakeCli := &scriptedDockerClient{exitCodes: []int{1, 1, 1, 1}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		wf.runWorkflow(ctx, fakeCli, types.NewPushedCommits())
		close(done)
	}()

	sendCycle := func(name string) {
		job, err := NewAIEJob("run_tests", &types.AIEngineResponse{
			PullRequest: pr,
			TestCmd:     []string{"pytest", name},
			TestName:    name,
			Tests:       []byte("package cycle"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !wf.trySend(job) {
			t.Fatalf("workflow stopped before accepting job %q", name)
		}
	}

	// First two attempts should reach the AI engine normally.
	sendCycle("first_test.go")
	<-received
	sendCycle("second_test.go")
	<-received

	// Third attempt should be rejected before touching Docker or the AI engine.
	sendCycle("third_test.go")

	select {
	case errObj := <-errCh:
		if errObj.wfid != wf.wfid {
			t.Errorf("ErrorObject.wfid = %d, want %d", errObj.wfid, wf.wfid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for max-attempts error")
	}

	select {
	case <-received:
		t.Fatal("AI engine should not have been called for the rejected attempt")
	case <-time.After(200 * time.Millisecond):
	}

	cancel()

	select {
	case <-received: // the close callback
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for close callback")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWorkflow did not exit after cancel")
	}
}
