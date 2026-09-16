package dockertools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	dockerClient "github.com/moby/moby/client"
)

type ImageManager interface {
	ImageList(ctx context.Context, options dockerClient.ImageListOptions) (dockerClient.ImageListResult, error)
	ImageRemove(ctx context.Context, tag string, options dockerClient.ImageRemoveOptions) (dockerClient.ImageRemoveResult, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options dockerClient.ImageBuildOptions) (dockerClient.ImageBuildResult, error)
}

type ContainerManager interface {
	ContainerList(ctx context.Context, options dockerClient.ContainerListOptions) (dockerClient.ContainerListResult, error)
	ContainerCreate(ctx context.Context, options dockerClient.ContainerCreateOptions) (dockerClient.ContainerCreateResult, error)
	ContainerRemove(ctx context.Context, containerID string, options dockerClient.ContainerRemoveOptions) (dockerClient.ContainerRemoveResult, error)
	ContainerLogs(ctx context.Context, containerID string, options dockerClient.ContainerLogsOptions) (dockerClient.ContainerLogsResult, error)
	ContainerStart(ctx context.Context, containerID string, options dockerClient.ContainerStartOptions) (dockerClient.ContainerStartResult, error)
	ContainerWait(ctx context.Context, containerID string, options dockerClient.ContainerWaitOptions) dockerClient.ContainerWaitResult
	ContainerInspect(ctx context.Context, containerID string, options dockerClient.ContainerInspectOptions) (dockerClient.ContainerInspectResult, error)
}

type DockerClient interface {
	ImageManager
	ContainerManager
}

type ContainerInspection struct {
	ExitCode  int
	StartTime time.Time
	EndTime   time.Time
	Errors    string
	OOMKilled bool
	Status    string
}

type TarBuilder interface {
	TarWorkspace(pw *io.PipeWriter, path string) error
}

var ImageBuildErr error = errors.New("Image build failed")

func WaitForDocker(ctx context.Context, cli *dockerClient.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond

	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := cli.Ping(pingCtx, dockerClient.PingOptions{})
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("docker daemon not reachable after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// Removes up old images
func ClearOldImages(ctx context.Context, im ImageManager) (err error) {
	images, err := im.ImageList(ctx, dockerClient.ImageListOptions{
		All:     true,
		Filters: dockerClient.Filters{"label": {"managed-by=ci-orchestrator": true}},
	})
	if err != nil {
		return fmt.Errorf("Failed to fetch image list: %w", err)
	}
	for _, item := range images.Items {
		if _, err := im.ImageRemove(ctx, item.ID, dockerClient.ImageRemoveOptions{}); err != nil {
			return fmt.Errorf("Failed to remove image %q: %w", item.ID, err)
		}
	}
	return nil
}

// Removes up old containers
func ClearOldContainers(ctx context.Context, cm ContainerManager) (err error) {
	conts, err := cm.ContainerList(ctx, dockerClient.ContainerListOptions{
		All:     true,
		Filters: dockerClient.Filters{"label": {"managed-by=ci-orchestrator": true}},
	})
	if err != nil {
		return fmt.Errorf("Failed to fetch container list: %w", err)
	}
	for _, item := range conts.Items {
		if _, err := cm.ContainerRemove(ctx, item.ID, dockerClient.ContainerRemoveOptions{}); err != nil {
			return fmt.Errorf("Failed to remove image %q: %w", item.ID, err)
		}
	}
	return nil
}

// Builds an image from src with sha as the tag.
func BuildImage(ctx context.Context, im ImageManager, wsName, sha, srcPath string, tb TarBuilder) (tag string, err error) {

	pr, pw := io.Pipe()
	defer func() {
		if closeErr := pr.Close(); closeErr != nil {
			err = closeErr
		}
	}()
	go func(w *io.PipeWriter, path string) {
		tarErr := tb.TarWorkspace(w, path)
		if tarErr != nil {
			slog.Error("Failed to tar the workspace", "error", tarErr)
			return
		}
		if err := w.CloseWithError(tarErr); err != nil {
			slog.Error("Pipe writter failed to close", "error", err)
			return
		}
	}(pw, srcPath)

	tag = fmt.Sprintf("%s:%s", wsName, sha)

	imageResult, err := im.ImageBuild(ctx, pr, dockerClient.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Labels:     map[string]string{"managed-by": "ci-orchestrator"},
	})
	if err != nil {
		return "", fmt.Errorf("Failed to build image: %w", err)
	}
	defer func() {
		if closeErr := imageResult.Body.Close(); closeErr != nil {
			err = closeErr
		}
	}()

	type temp struct {
		ErrorDetail struct {
			Code    int    `json:"code,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"errorDetail,omitempty"`
	}

	var t temp
	jsonDecoder := json.NewDecoder(imageResult.Body)
	if err := jsonDecoder.Decode(&t); err != nil {
		return "", fmt.Errorf("Failed to decode image build result: %w", err)
	}

	if t.ErrorDetail.Code != 0 || t.ErrorDetail.Message != "" {
		return "", fmt.Errorf(
			"Failed to build image\n"+
				"Code: %v\n"+
				"Message: %q\n"+
				"Error: %w",
			t.ErrorDetail.Code, t.ErrorDetail.Message, ImageBuildErr)
	}

	return tag, err
}

// Builds and runs a container labeled with tag. Returns the id, stdout, stderr, and an error.
// The caller is responsible for closing the logs and removing the container.
func RunContainer(ctx context.Context, cm ContainerManager, tag string, cmd []string) (id string, outReader io.ReadCloser, errReader io.ReadCloser, err error) {

	cont, err := cm.ContainerCreate(ctx, dockerClient.ContainerCreateOptions{
		Config: &container.Config{
			Cmd:    cmd,
			Env:    config.TestingEnvSlice,
			Labels: map[string]string{"managed-by": "ci-orchestrator"},
		},
		HostConfig: &container.HostConfig{
			Memory: int64(config.ContainerMemoryCap * config.MB),
		},
		Name:  tag,
		Image: tag,
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("Failed to create container %q: %w", tag, err)
	}

	id = cont.ID

	logs, err := cm.ContainerLogs(ctx, id, dockerClient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("Failed to create a container logger: %w", err)
	}

	if _, err := cm.ContainerStart(ctx, id, dockerClient.ContainerStartOptions{}); err != nil {
		return "", nil, nil, fmt.Errorf("Failed to start container: %w", err)
	}

	response := cm.ContainerWait(ctx, id, dockerClient.ContainerWaitOptions{})
	select {
	case <-response.Result:
		slog.Info("Container completed")
	case err := <-response.Error:
		return "", nil, nil, fmt.Errorf("Container failed: %w", err)
	}

	outReader, outWriter := io.Pipe()
	errReader, errWriter := io.Pipe()

	go func() {
		_, copyErr := stdcopy.StdCopy(outWriter, errWriter, logs)
		outWriter.CloseWithError(copyErr)
		errWriter.CloseWithError(copyErr)
	}()

	return id, outReader, errReader, err
}

// Returns current state of a completed container as a ContainerInspection struct.
func InspectContainer(ctx context.Context, cm ContainerManager, id string) (inspection ContainerInspection, err error) {
	cont, err := cm.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
	if err != nil {
		return ContainerInspection{}, fmt.Errorf("Failed to inspect container %q: %w", id, err)
	}
	contState := cont.Container.State
	start, err := time.Parse(time.RFC3339Nano, contState.StartedAt)
	if err != nil {
		return ContainerInspection{}, fmt.Errorf("Failed to parse start time: %w", err)
	}
	end, err := time.Parse(time.RFC3339Nano, contState.FinishedAt)
	if err != nil {
		return ContainerInspection{}, fmt.Errorf("Failed to parse end time: %w", err)
	}
	inspection = ContainerInspection{
		ExitCode:  contState.ExitCode,
		StartTime: start,
		EndTime:   end,
		Errors:    contState.Error,
		OOMKilled: contState.OOMKilled,
		Status:    string(contState.Status),
	}
	return inspection, err
}

// Removes the specified container.
func RemoveContainer(ctx context.Context, cm ContainerManager, id string) (err error) {
	cont, err := cm.ContainerInspect(ctx, id, dockerClient.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("Failed to inspect container %q: %w", id, err)
	}

	// Removes volumes for now
	if _, err := cm.ContainerRemove(ctx, cont.Container.ID, dockerClient.ContainerRemoveOptions{RemoveVolumes: true}); err != nil {
		return fmt.Errorf("Failed to remove container: %w", err)
	}
	return nil
}

// Removes the specified image.
func RemoveImage(ctx context.Context, im ImageManager, tag string) (err error) {
	if _, err := im.ImageRemove(ctx, tag, dockerClient.ImageRemoveOptions{}); err != nil {
		return fmt.Errorf("Failed to remove container: %w", err)
	}
	return nil
}
