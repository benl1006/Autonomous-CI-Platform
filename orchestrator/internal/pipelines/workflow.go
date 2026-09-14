package pipelines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/dockertools"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/servertools"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/wstools"
)

type Workflow struct {
	wfid             int // The pr number.
	pullRequest      *types.PullRequest
	jobs             chan Job
	workspace        Workspace
	workspaceMutex   sync.RWMutex
	attemptNum       int
	currentTestsPath string
	errorChannel     chan<- ErrorObject
	done             chan struct{}
}

// Contains information associated with a particular workspace. Protected by a mutex.
type Workspace struct {
	path            string       // Path to the associated workspace.
	removeWorkspace func() error // Removes the workspace at path.
}

type Job struct {
	// Can be one of:
	// - "open"
	// - "edit"
	// - "sync"
	// - "run_tests"
	// - "commit_push"
	JobType string

	// Only one of:

	Aier        *types.AIEngineResponse
	PullRequest *types.PullRequest
}

// Creates a new workflow. Path, cleanWs, and cancelWf function are are uninitialized by default.
// Path and cleanup are initialized by the OPEN job.
func newWorkflow(pr *types.PullRequest, errChan chan<- ErrorObject) *Workflow {
	return &Workflow{
		wfid:         pr.Number,
		pullRequest:  pr,
		jobs:         make(chan Job),
		attemptNum:   0,
		errorChannel: errChan,
		done:         make(chan struct{}),
	}
}

func (wf *Workflow) GetPath() string {
	wf.workspaceMutex.RLock()
	defer wf.workspaceMutex.RUnlock()
	return wf.workspace.path
}

func (wf *Workflow) SetPath(p string) {
	wf.workspaceMutex.Lock()
	defer wf.workspaceMutex.Unlock()
	wf.workspace.path = p
}

func (wf *Workflow) GetCleanWorkspace() func() error {
	wf.workspaceMutex.RLock()
	defer wf.workspaceMutex.RUnlock()
	return wf.workspace.removeWorkspace
}

func (wf *Workflow) SetCleanWorkspace(cws func() error) {
	wf.workspaceMutex.Lock()
	defer wf.workspaceMutex.Unlock()
	wf.workspace.removeWorkspace = cws
}

func (wf *Workflow) trySend(job Job) (delivered bool) {
	select {
	case wf.jobs <- job:
		return true
	case <-wf.done:
		return false // workflow has exited; job dropped, caller decides what to do
	}
}

func (wf *Workflow) isRunning() bool {
	select {
	case <-wf.done:
		return false
	default:
		return true
	}
}

// Starts the job pipeline. Handles incoming jobs. Blocks until an error occurs.
func (wf *Workflow) runWorkflow(ctx context.Context, cli dockertools.DockerClient, pc *types.PushedCommits) {
	defer close(wf.done)
	for {
		select {
		case <-ctx.Done():
			if wf.workspace.removeWorkspace != nil {
				if err := wf.workspace.removeWorkspace(); err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to clean up workspace: %w", err),
					}
					return
				}
			}
			newCtx, cancel := context.WithTimeout(context.Background(), time.Duration(config.RequestCloseTimeout)*time.Second)
			defer cancel()
			if err := servertools.SendRequestAIEngine(newCtx, "close", types.AIEngineRequest{Wfid: wf.wfid}); err != nil {
				wf.errorChannel <- ErrorObject{
					wfid: wf.wfid,
					err:  fmt.Errorf("Failed to send request to ai engine: %w", err),
				}
				return
			}
			return

		case job := <-wf.jobs:
			switch job.JobType {
			case "open":
				wf.attemptNum = 0
				path, clean, err := wstools.InitWorkspace(ctx, *wf.pullRequest, &wstools.GithubClient{})
				if err != nil {
					var cleanerr error
					if clean != nil {
						cleanerr = clean()
					}
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to create a temporary workspace: %w", errors.Join(err, cleanerr)),
					}
					continue
				}
				wf.workspace = Workspace{
					path:            path,
					removeWorkspace: clean,
				}

				if err = servertools.SendRequestAIEngine(ctx, "open", types.AIEngineRequest{
					Wfid:        wf.wfid,
					PullRequest: *wf.pullRequest,
					RepoUrl:     config.RepositoryUrl,
				}); err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to send request to ai engine: %w", err),
					}
					continue
				}

			case "edit", "sync":
				wf.attemptNum = 0
				pr := job.PullRequest
				if pr == nil {
					panic("EDIT or SYNC should always come from a pull request.")
				}

				wf.pullRequest = pr

				// May be redundant, but exists just in case the types are relabled.
				var jt string
				if job.JobType == "edit" {
					jt = "edit"
				} else {
					jt = "sync"
				}

				if err := servertools.SendRequestAIEngine(ctx, jt, types.AIEngineRequest{
					Wfid:        wf.wfid,
					PullRequest: *pr,
				}); err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to send request to ai engine: %w", err),
					}
					continue
				}

			case "run_tests":
				aier := job.Aier
				if aier == nil {
					panic("RUN_TESTS should always come from a pull request.")
				}
				if aier.PullRequest != *wf.pullRequest {
					// Drop aier response if the pull requests do not match by value
					continue
				}
				if wf.attemptNum >= config.MaxTestPatchingAttempts {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Test generation failed: too many attempts"),
					}
					continue
				}
				wf.attemptNum++

				testPath := filepath.Clean(aier.TestName)
				if filepath.IsAbs(testPath) || testPath == ".." || strings.HasPrefix(testPath, ".."+string(filepath.Separator)) {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("invalid generated test path: %q", aier.TestName),
					}
					continue
				}

				if err := wstools.InsertTests(filepath.Join(wf.workspace.path, testPath), aier.Tests); err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to insert tests: %w", err),
					}
					continue
				}
				nameFormatter := strings.NewReplacer("/", "-", "|", "-", "<", "-", ">", "-", "\"", "-")
				wsName := nameFormatter.Replace(fmt.Sprintf("%s-%v", wf.pullRequest.Branch, wf.wfid))
				tag, err := dockertools.BuildImage(ctx, cli, wsName, wf.pullRequest.HeadSHA, wf.workspace.path, &dockertools.RealTarBuilder{})
				if err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to build image: %w", err),
					}
					continue
				}

				// Process the container
				contInspect, logOut, logErr, err := processContainer(ctx, tag, aier.TestCmd, cli)
				if err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Container failed: %w", err),
					}
					continue
				}

				if err := dockertools.RemoveImage(ctx, cli, tag); err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to remove image: %w", err),
					}
					continue
				}

				if err := servertools.SendRequestAIEngine(ctx, "logs", types.AIEngineRequest{
					Wfid:        wf.wfid,
					PullRequest: *wf.pullRequest,
					RepoUrl:     config.RepositoryUrl,
					Stdout:      logOut,
					Stderr:      logErr,
					StartTime:   contInspect.StartTime,
					EndTime:     contInspect.EndTime,
					Errors:      contInspect.Errors,
					Status:      contInspect.Status,
					OOMKilled:   contInspect.OOMKilled,
					ExitCode:    contInspect.ExitCode,
				}); err != nil {
					if errors.Is(err, context.Canceled) && ctx.Err() != nil {
						continue
					}
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Request to AI Engine failed: %w", err),
					}
					continue
				}

			case "commit_push":
				aier := job.Aier
				if aier == nil {
					panic("RUN_TESTS should always come from a pull request.")
				}
				if err := servertools.PostSummaryComment(ctx, wf.pullRequest.CommentsURL, aier.Summary); err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to write summary comment: %w", err),
					}
					continue
				}
				newSha, err := wf.SendUpdatesToRemote(&wstools.GithubClient{})
				if err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to update remote: %w", err),
					}
				}
				pc.Add(wf.wfid, newSha)

			default:
				panic(fmt.Sprintf("Unsupported job type: %v", job))
			}
		}
	}
}

// Creates a container, runs it, and removes it. Returns a ContainerInspection, stdout, stderr, and an error.
func processContainer(ctx context.Context, tag string, cmd []string, cli dockertools.DockerClient) (inspect dockertools.ContainerInspection, logOutString string, logErrString string, err error) {
	subContext, cancel := context.WithTimeout(ctx, time.Duration(config.ContainerTimeout)*time.Minute)
	defer cancel()
	contID, logOut, logErr, err := dockertools.RunContainer(subContext, cli, tag, cmd)
	if err != nil {
		return dockertools.ContainerInspection{}, "", "", fmt.Errorf("Failed to build container: %w", err)
	}

	// Close the logs and remove container
	defer func() {
		if closeErr := logOut.Close(); closeErr != nil {
			err = fmt.Errorf("Failed to close out logs: %w", closeErr)
		}
		if closeErr := logErr.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("Failed to close error logs: %w", closeErr))
		}
		if removeErr := dockertools.RemoveContainer(ctx, cli, contID); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("Failed to remove container: %w", removeErr))
		}
	}()

	logOutBytes, err := io.ReadAll(logOut)
	if err != nil {
		return dockertools.ContainerInspection{}, "", "", fmt.Errorf("Failed to read output logs: %w", err)
	}
	logOutString = string(logOutBytes)
	logErrBytes, err := io.ReadAll(logErr)
	if err != nil {
		return dockertools.ContainerInspection{}, "", "", fmt.Errorf("Failed to read error logs: %w", err)
	}
	logErrString = string(logErrBytes)

	inspect, err = dockertools.InspectContainer(ctx, cli, contID)
	if err != nil {
		return dockertools.ContainerInspection{}, "", "", fmt.Errorf("Failed to inspect container: %w", err)
	}

	return inspect, logOutString, logErrString, err
}

// Adds, commits, and pushes current workspace state to remote.
func (wf *Workflow) SendUpdatesToRemote(cli wstools.GitClient) (newSha string, err error) {
	newSha, err = cli.AddAllCommitPush("", wf.workspace.path, wf.pullRequest.Branch)
	if err != nil {
		return "", fmt.Errorf("Failed to add, commit, and push changes: %w", err)
	}
	return newSha, nil
}
