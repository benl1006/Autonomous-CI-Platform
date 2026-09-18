package types

import ()

type ChangedFile struct {
	Path     string `json:"path"`
	Contents []byte `json:"contents"`
}
