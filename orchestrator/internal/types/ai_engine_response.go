package types

// The response from the AI Engine.
// Response should come with a HMAC-Signature-256 header.
// Contains the tests and the summary.
type AIEngineResponse struct {
	Wfid        int         `json:"wfid"`
	PullRequest PullRequest `json:"pullRequest"`

	// Tests are ignored if Done.
	TestCmd  []string      `json:"testCmd"`
	Tests    []ChangedFile `json:"tests"`

	// Done should always be accompanied by Summary.
	Done    bool   `json:"done"`
	Summary string `json:"summary"`
}
