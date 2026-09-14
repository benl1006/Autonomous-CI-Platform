package wstools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
)

type GitClient interface {
	// Initializes the repository in the directory at path.
	InitRepo(ctx context.Context, path string, pr types.PullRequest) (err error)
	// Adds, commits, and pushes changes to remote. Returns the new sha and an error.
	AddAllCommitPush(commitMsg, wsPath, branch string) (string, error)
}

// Creates a unique temp directory in workspaceDir
func tempWorkspace(sha string) (path string, cleanup func() error, err error) {
	path, err = os.MkdirTemp(config.WsDir, fmt.Sprintf("sha%s_*", sha))
	if err != nil {
		return "", nil, err
	}
	cleanup = func() error {
		if cleanupErr := os.RemoveAll(path); cleanupErr != nil {
			return fmt.Errorf("Failed remove the temporary directory %s: %w", path, cleanupErr)
		}
		return nil
	}
	return path, cleanup, nil
}

// Initializes the workspace and clones it.
func InitWorkspace(ctx context.Context, pr types.PullRequest, cli GitClient) (path string, cleanup func() error, err error) {

	path, cleanup, err = tempWorkspace(pr.HeadSHA)
	if err != nil {
		return "", nil, fmt.Errorf("Failed to create a temporary workspace: %w", err)
	}

	if err := cli.InitRepo(ctx, path, pr); err != nil {
		cleanerr := cleanup()
		if cleanerr != nil {
			return "", nil, fmt.Errorf("Failed to clean up workspace: %w", cleanerr)
		}
		return "", nil, fmt.Errorf("Failed to checkout commit: %v", err)
	}

	slog.Info("Successfully checked out specific SHA natively!")
	return path, cleanup, nil
}

// Clears the temp_workspace directory
func ClearWorkspaces() (err error) {

	// Remove everything including the root dir
	if err := os.RemoveAll(config.WsDir); err != nil {
		return fmt.Errorf("Failed to remove dir: %w", err)
	}

	// Recreate the empty directory
	if err := os.MkdirAll(config.WsDir, os.FileMode(0755)); err != nil {
		return fmt.Errorf("Failed to recreate dir: %w", err)
	}

	return nil
}

// Parses and inserts tests.
func InsertTests(path string, data []byte) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("Failed to create test directory: %w", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("Failed to create test file: %w", err)
	}
	defer file.Close()

	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("Failed to write tests: %w", err)
	}

	return nil
}
