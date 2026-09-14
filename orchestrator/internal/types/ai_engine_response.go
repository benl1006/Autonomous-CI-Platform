package types

// The response from the AI Engine.
// Response should come with a HMAC-Signature-256 header.
// Contains the tests and the summary.
//
// json tags are pinned to the current field names on purpose: the AI Engine
// (orchestrator_client.py) builds this payload as a plain dict with these
// exact capitalised keys, so renaming a Go field here without updating the
// tag would silently break unmarshalling.
type AIEngineResponse struct {
	Wfid        int         `json:"Wfid"`
	PullRequest PullRequest `json:"PullRequest"`

	// Tests are ignored if Done.
	TestCmd  []string `json:"TestCmd"`
	TestName string   `json:"TestName"`
	Tests    []byte   `json:"Tests"`

	// Done should always be accompanied by Summary.
	Done    bool   `json:"Done"`
	Summary string `json:"Summary"`
}
