package pipelines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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
	wfid             int // The pull request number.
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
	// Must be in config.AIEJobTypes or config.WebhookJobTypes
	jobType string

	// Only one of:
	aier        *types.AIEngineResponse
	pullRequest *types.PullRequest
}

var firstOpen = true // remove when persistance is added

// Creates the specidied AI Engine job. Errors if jt is an invalid job type.
func NewAIEJob(jt string, resp *types.AIEngineResponse) (Job, error) {
	if !slices.Contains(config.AiEngineResponseJobTypes, jt) {
		return Job{}, errors.New("Invalid job type for AIE job: " + jt)
	}
	return Job{
		jobType: jt,
		aier:    resp,
	}, nil
}

// Creates the specified pull request job. Errors if jt is an invalid job type.
func NewPullRequestJob(jt string, pr *types.PullRequest) (Job, error) {
	if !slices.Contains(config.WebhookJobTypes, jt) {
		return Job{}, errors.New("Invalid job type for Pull Request Job: " + jt)
	}
	return Job{
		jobType:     jt,
		pullRequest: pr,
	}, nil
}

func (j *Job) GetJobType() string {
	return j.jobType
}

func (j *Job) GetAIER() *types.AIEngineResponse {
	return j.aier
}
func (j *Job) GetPullRequest() *types.PullRequest {
	return j.pullRequest
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

func (wf *Workflow) resetDone() {
	wf.done = make(chan struct{})
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
			switch job.GetJobType() {
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

				changedFilePaths, err := func(ctx context.Context, owner, repoName string, prNum int) ([]string, error) {
					newCtx, cancel := context.WithTimeout(ctx, time.Duration(config.RequestTimeout))
					defer cancel()
					changedFilePaths, err := wstools.GetChangedFilePaths(newCtx, owner, repoName, prNum)
					if err != nil {
						return nil, err
					}
					return changedFilePaths, nil
				}(ctx, wf.pullRequest.Owner, wf.pullRequest.RepoName, wf.wfid)
				if err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to get the changed file paths: %w", err),
					}
					continue
				}

				changedFiles, err := wstools.ReadFiles(wf.workspace.path, changedFilePaths)
				if err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to read changed files %s from workspace: %w", changedFilePaths, err),
					}
					continue
				}

				if err = servertools.SendRequestAIEngine(ctx, "open", types.AIEngineRequest{
					Wfid:         wf.wfid,
					PullRequest:  *wf.pullRequest,
					ChangedFiles: changedFiles,
				}); err != nil {
					wf.errorChannel <- ErrorObject{
						wfid: wf.wfid,
						err:  fmt.Errorf("Failed to send request to AI Engine: %w", err),
					}
					continue
				}

			case "edit", "sync":
				wf.attemptNum = 0
				pr := job.GetPullRequest()
				if pr == nil {
					panic("EDIT or SYNC should always come from a pull request.")
				}

				wf.pullRequest = pr

				// May be redundant, but exists just in case the types are relabled.
				var jt string
				if job.GetJobType() == "edit" {
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
				aier := job.GetAIER()
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

				if err := wstools.InsertTests(wf.workspace.path, aier.Tests); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						servertools.SendRequestAIEngine(ctx, "test_results", types.AIEngineRequest{
							Wfid:        wf.wfid,
							PullRequest: *wf.pullRequest,
							Error:       err.Error(),
						})
					} else {
						wf.errorChannel <- ErrorObject{
							wfid: wf.wfid,
							err:  fmt.Errorf("Failed to insert tests: %w", err),
						}
						continue
					}
				}
				nameFormatter := strings.NewReplacer("/", "-", "|", "-", "<", "-", ">", "-", "\"", "-")
				wsName := nameFormatter.Replace(fmt.Sprintf("%s-%v", wf.pullRequest.Branch, wf.wfid))
				tag, err := dockertools.BuildImage(ctx, cli, wsName, wf.pullRequest.HeadSHA, wf.workspace.path, &wstools.RealTarBuilder{})
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

				if err := servertools.SendRequestAIEngine(ctx, "test_results", types.AIEngineRequest{
					Wfid:        wf.wfid,
					PullRequest: *wf.pullRequest,
					TestResults: types.TestResults{
						Stdout:    logOut,
						Stderr:    logErr,
						StartTime: contInspect.StartTime,
						EndTime:   contInspect.EndTime,
						Errors:    contInspect.Errors,
						Status:    contInspect.Status,
						OOMKilled: contInspect.OOMKilled,
						ExitCode:  contInspect.ExitCode,
					},
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
				aier := job.GetAIER()
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
