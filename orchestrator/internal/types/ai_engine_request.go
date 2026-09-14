package types

import (
	"time"
)

// The request sent to the AI Engine.
// Request should come with a HMAC-Signature-256.
// Contains the pull request and the logs.
//
// json tags are pinned to the current field names on purpose: the AI Engine's
// JobRequest (Pydantic) has no aliasing and expects these exact keys, so
// renaming a Go field here without updating the tag would silently break it.
type AIEngineRequest struct {
	// Mandatory
	Wfid        int         `json:"Wfid"`
	PullRequest PullRequest `json:"PullRequest"`

	// Test results (optional; leave blank if not sending logs)
	Stdout    string    `json:"Stdout"`
	Stderr    string    `json:"Stderr"`
	StartTime time.Time `json:"StartTime"`
	EndTime   time.Time `json:"EndTime"`
	Errors    string    `json:"Errors"` // Compile or entry command errors
	Status    string    `json:"Status"` // One of "created", "running", "paused", "restarting", "removing", "exited", or "dead"
	OOMKilled bool      `json:"OOMKilled"` // Killed: out of memory
	ExitCode  int       `json:"ExitCode"`
}
