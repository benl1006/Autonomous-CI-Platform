package types

import (
	"time"
)

// The request sent to the AI Engine.
// Request should come with a HMAC-Signature-256.
// Contains the pull request and the logs.
type AIEngineRequest struct {
	// Mandatory
	Wfid        int         `json:"wfid"`
	PullRequest PullRequest `json:"pullRequest"`

	// Changed files (optional)
	Files []FileDiff `json:"changedFiles"`

	// Test results (optional; leave blank if not sending logs)
	TestResults TestResults `json:"testResults"`

	// Workflow errored before running the container (optional)
	Error string `json:"error"`
}

// Contains the test results.
type TestResults struct {
	Stdout    string    `json:"stdout"`
	Stderr    string    `json:"stderr"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
	Errors    string    `json:"errors"`    // Compile or entry command errors
	Status    string    `json:"status"`    // One of "created", "running", "paused", "restarting", "removing", "exited", or "dead"
	OOMKilled bool      `json:"oomKilled"` // Killed: out of memory
	ExitCode  int       `json:"exitCode"`
}
