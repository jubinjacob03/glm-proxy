package zbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// fetchModelsFromZAI retrieves models from Z.AI /api/models,
// keeping only glm-4.7 and newer (the API returns newest-first).
func fetchModelsFromZAI() []ModelInfo {
	modelsCacheMu.Lock()
	defer modelsCacheMu.Unlock()

	if len(modelsCache) > 0 && time.Since(modelsCacheTime) < modelsCacheTTL {
		return modelsCache
	}
	// Stamped before the call so a failing upstream is rate-limited by the
	// same TTL as a successful one. Otherwise every request retries the 15s
	// fetch while holding this lock, serialising all traffic behind it.
	modelsCacheTime = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL+"/api/models", nil)
	if err != nil {
		logError("fetchModels request: " + err.Error())
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}
	session.mu.Lock()
	token := session.Token
	session.mu.Unlock()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("User-Agent", zaiUserAgent)
	resp, err := zaiHTTPClient.Do(req)
	if err != nil {
		logError("fetchModels do: " + err.Error())
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logError(fmt.Sprintf("fetchModels status: %d", resp.StatusCode))
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}

	var apiResp struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Info struct {
				Name string `json:"name"`
				Meta struct {
					Description  string                 `json:"description"`
					Capabilities map[string]interface{} `json:"capabilities"`
				} `json:"meta"`
			} `json:"info"`
		} `json:"data"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&apiResp); err != nil {
		logError("fetchModels parse: " + err.Error())
		if len(modelsCache) > 0 {
			return modelsCache
		}
		return fallbackModels
	}

	filtered := make([]ModelInfo, 0, len(apiResp.Data))
	for _, m := range apiResp.Data {
		filtered = append(filtered, ModelInfo{
			ID:           m.ID,
			Name:         m.Name,
			Description:  m.Info.Meta.Description,
			Capabilities: m.Info.Meta.Capabilities,
		})
		if m.ID == "glm-4.7" {
			break // inclusive cutoff
		}
	}

	if len(filtered) > 0 {
		modelsCache = filtered
		modelsCacheTime = time.Now()
		logInfo(fmt.Sprintf("Fetched %d models from Z.AI", len(filtered)))
	}

	if len(modelsCache) > 0 {
		return modelsCache
	}
	return fallbackModels
}

// getModelCapabilities returns the raw capabilities map for a model.
func getModelCapabilities(modelID string) map[string]interface{} {
	for _, m := range fetchModelsFromZAI() {
		if strings.EqualFold(m.ID, modelID) {
			return m.Capabilities
		}
	}
	return nil
}

// modelSupportsReasoningEffort requires the capability to be explicitly true.
// A false value or a missing field both mean unsupported.
func modelSupportsReasoningEffort(modelID string) bool {
	if modelID == "" {
		return false
	}
	caps := getModelCapabilities(modelID)
	if caps == nil {
		return false
	}
	v, ok := caps["reasoning_effort"].(bool)
	return ok && v
}

// isValidReasoningEffort accepts only "high" and "max".
func isValidReasoningEffort(value string) bool {
	switch value {
	case "high", "max":
		return true
	default:
		return false
	}
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := fetchModelsFromZAI()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]interface{}{
			"id":           m.ID,
			"object":       "model",
			"created":      now,
			"owned_by":     "z-ai",
			"display_name": m.Name,
			"description":  m.Description,
		})
	}
	writeJSON(w, 200, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func modelsHandler2(w http.ResponseWriter, r *http.Request) {
	models := fetchModelsFromZAI()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	currentModel := "glm-5.2"
	if len(ids) > 0 {
		currentModel = ids[0]
	}
	writeJSON(w, 200, map[string]interface{}{
		"models":       ids,
		"currentModel": currentModel,
	})
}
