package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/janit/viiwork/internal/model"
)

// Re-export for backward compatibility with existing tests
type ModelEntry = model.ModelEntry
type ModelsResponse = model.ModelsResponse

func ModelIDFromPath(modelPath string) string {
	return model.IDFromPath(modelPath)
}

func NewModelsHandler(modelPath string) http.Handler {
	modelID := model.IDFromPath(modelPath)
	resp := model.ModelsResponse{
		Object: "list",
		Data:   []model.ModelEntry{{ID: modelID, Object: "model", OwnedBy: "local"}},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}
