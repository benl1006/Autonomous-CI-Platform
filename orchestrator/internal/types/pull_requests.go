package types

import (
	"encoding/json"
	"fmt"
)

type PullRequest struct {
	RepoName    string `json:"repoName"`
	Owner       string `json:"repoFullName"`
	Number      int    `json:"number"`
	Action      string `json:"action"`
	Branch      string `json:"branch"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HeadSHA     string `json:"headsha"`
	BaseSHA     string `json:"basesha"`
	Merged      bool   `json:"merged"`
	CommentsURL string `json:"url"`
}

// Populates fields from a byte slice
func (pr *PullRequest) UnmarshalPullRequest(data []byte) (err error) {
	var temp struct {
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Number      int    `json:"number"`
		Action      string `json:"action"`
		PullRequest struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  struct {
				Ref string `json:"ref"`
				Sha string `json:"sha"`
			} `json:"head"`
			Base struct {
				Sha string `json:"sha"`
			} `json:"base"`
			Merged      bool   `json:"merged"`
			CommentsURL string `json:"comments_url"`
		} `json:"pull_request"`
	}

	if err := json.Unmarshal(data, &temp); err != nil {
		return fmt.Errorf("Failed to unmarshal json data: %w", err)
	}

	pr.RepoName = temp.Repository.Name
	pr.Owner = temp.Repository.Owner.Login
	pr.Number = temp.Number
	pr.Action = temp.Action
	pr.Branch = temp.PullRequest.Head.Ref
	pr.Title = temp.PullRequest.Title
	pr.Body = temp.PullRequest.Body
	pr.HeadSHA = temp.PullRequest.Head.Sha
	pr.BaseSHA = temp.PullRequest.Base.Sha
	pr.Merged = temp.PullRequest.Merged
	pr.CommentsURL = temp.PullRequest.CommentsURL

	return nil
}
