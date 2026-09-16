package wstools

import (
	"context"
	"errors"
	"fmt"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
	"github.com/go-git/go-git/v6"
	gitConfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitPlumbingClient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/google/go-github/v92/github"

)

type GithubClient struct{}

const localBranch = "temp-branch"

// Initializes the repository in the directory at path.
func (c *GithubClient) InitRepo(ctx context.Context, path string, pr types.PullRequest) (err error) {
	repo, err := git.PlainInit(path, false)
	if err != nil {
		return fmt.Errorf("Failed to initialize repo on branch %q: %w", pr.Branch, err)
	}

	if _, err = repo.CreateRemote(&gitConfig.RemoteConfig{
		Name: "origin",
		URLs: []string{config.RepositoryUrl},
	}); err != nil {
		return fmt.Errorf("Failed to create remote on branch %q: %w", pr.Branch, err)
	}

	if _, err := c.updateWorkspace(ctx, path, pr); err != nil {
		return fmt.Errorf("Failed to update the workspace: %w", err)
	}

	return nil
}

// Fetches and checksout the latest changes from remote.
func (c *GithubClient) updateWorkspace(ctx context.Context, wsPath string, pr types.PullRequest) (branchHeadSHA string, err error) {
	repo, err := git.PlainOpen(wsPath)
	if err != nil {
		return "", fmt.Errorf("Failed to open the repository at %s: %w", wsPath, err)
	}

	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteURL:     config.RepositoryUrl,
		ClientOptions: []gitPlumbingClient.Option{gitPlumbingClient.WithHTTPAuth(&http.BasicAuth{Username: config.GithubToken})},
		RefSpecs:      []gitConfig.RefSpec{gitConfig.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", pr.Branch, localBranch))},
		Depth:         1,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", fmt.Errorf("Failed to fetch branch %s: %w", pr.Branch, err)
	}

	if err := checkoutBranch(repo, localBranch); err != nil {
		return "", fmt.Errorf("Failed to checkout branch %s: %w", localBranch, err)
	}

	ref, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("Failed to get HEAD: %w", err)
	}

	return ref.Hash().String(), nil
}

// Adds, commits, and pushes changes to remote. Returns the new sha and an error.
func (c *GithubClient) AddAllCommitPush(commitMsg, wsPath, branch string) (newSha string, err error) {

	repo, err := git.PlainOpen(wsPath)
	if err != nil {
		return "", fmt.Errorf("Failed to open the repository at %s: %w", wsPath, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("Failed to get worktree: %w", err)
	}

	if err := checkoutBranch(repo, localBranch); err != nil {
		return "", fmt.Errorf("Failed to checkout branch %s: %w", localBranch, err)
	}

	hash, err := wt.Commit(commitMsg, &git.CommitOptions{All: true})
	if err != nil {
		return "", fmt.Errorf("Failed to commit changes: %w", err)
	}
	newSha = hash.String()

	remote, err := repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("Failed to get remote: %w", err)
	}

	if err := repo.Push(&git.PushOptions{
		RemoteName:    "origin",
		RemoteURL:     remote.Config().URLs[0],
		ClientOptions: []gitPlumbingClient.Option{gitPlumbingClient.WithHTTPAuth(&http.BasicAuth{Username: config.GithubToken})},
		RefSpecs:      []gitConfig.RefSpec{gitConfig.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", localBranch, branch))},
		Force:         true,
	}); err != nil {
		return "", fmt.Errorf("Failed to push changes: %w", err)
	}
	return newSha, nil
}

func checkoutBranch(repo *git.Repository, ref string) (err error) {
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("Failed to get worktree: %w", err)
	}

	if err = wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(ref),
		Force:  true,
	}); err != nil {
		return fmt.Errorf("Failed to checkout: %w", err)
	}
	return nil
}

// Get the file paths of the changed files.
func GetChangedFilePaths(ctx context.Context, owner, repo string, prNum int) (changedFilePaths []string, err error) {
	client, err := github.NewClient(github.WithAuthToken(config.GithubToken))
	if err != nil {
		return nil, fmt.Errorf("Failed to create Github client: %w", err)
	}
	opts := &github.ListOptions{
		PerPage: 100,
		Page: 1,
	}
	for {
		files, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, prNum, opts)
		if err != nil {
			return nil, fmt.Errorf("Failed to list pull request files: %w", err)
		}
		for _, file := range files {
			if file.Filename != nil {
				changedFilePaths = append(changedFilePaths, *file.Filename)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return changedFilePaths, nil
}