package types

type FileDiff struct {
	Path     string `json:"path"`
	Contents []byte `json:"contents"`
}
