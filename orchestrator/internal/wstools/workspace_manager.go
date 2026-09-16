package wstools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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

// Parses and inserts tests to wsPath.
func InsertTests(wsPath string, tests []types.ChangedFile) (err error) {

	for _, test := range tests {
		testPath, err := resolveInBase(wsPath, test.Path)
		if err != nil {
			return fmt.Errorf("Failed to resolve path %s from %s: %w", test.Path, wsPath, err)
		}
		file, err := os.Create(testPath)
		if err != nil {
			return fmt.Errorf("Failed to create test file at %s: %w", testPath, err)
		}
		defer file.Close()

		if _, err := file.Write(test.Contents); err != nil {
			return fmt.Errorf("Failed to write tests: %w", err)
		}
	}

	return nil
}

// Returns a slice of types.ChangedFiles from a slice of paths.
func ReadChangedFiles(wsPath string, changedFilePaths []string) (changedFiles []types.ChangedFile, err error) {
	for _, path := range changedFilePaths {
		cleanPath, err := resolveInBase(wsPath, path)
		if err != nil {
			return nil, fmt.Errorf("Failed to resolve path %s from %s: %w", cleanPath, wsPath, err)
		}
		fileContents, err := os.ReadFile(cleanPath)
		if err != nil {
			return nil, fmt.Errorf("Failed to read file: %w", err)
		}
		changedFiles = append(changedFiles, types.ChangedFile{
			Path: path,
			Contents: fileContents,
		})
	}

	return changedFiles, nil
}


// Returns a clean path and ensures the path stays inside the base.
func resolveInBase(base, rel string) (path string, err error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("Path must be relative: %q", rel)
	}

	joined := filepath.Join(base, rel)
	cleanBase := filepath.Clean(base)

	baseAndSep := cleanBase + string(filepath.Separator)
	if joined != cleanBase && !strings.HasPrefix(joined, baseAndSep) {
		return "", fmt.Errorf("Path %q escapes workspace root %q", rel, base)
	}
	return joined, nil
}