package model

import (
	"path/filepath"
	"strings"
)

type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

func IDFromPath(modelPath string) string {
	base := filepath.Base(modelPath)
	return strings.TrimSuffix(base, ".gguf")
}
