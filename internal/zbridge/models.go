package zbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// fetchModelsFromZAI returns the model list, refreshing at most once per TTL. Every
// completion calls this via getModelCapabilities, so the 15s refresh must not hold the
// lock — that stalled all concurrent requests once per TTL. Callers arriving mid
// refresh get the previous list instead of waiting.
func fetchModelsFromZAI() []ModelInfo {
	modelsCacheMu.Lock()
	stale := modelsCache
	if len(stale) > 0 && time.Since(modelsCacheTime) < modelsCacheTTL {
		modelsCacheMu.Unlock()
		return stale
	}
	if modelsRefreshing {
		modelsCacheMu.Unlock()
		if len(stale) > 0 {
			return stale
		}
		return fallbackModels
	}
	// Stamped before the fetch so a failing upstream is rate-limited by the same
	// TTL as a success, rather than retried by every request.
	modelsRefreshing = true
	modelsCacheTime = time.Now()
	modelsCacheMu.Unlock()

	if len(stale) > 0 {
		go refreshModelsCache()
		return stale
	}
	return refreshModelsCache()
}

func refreshModelsCache() []ModelInfo {
	var fetched []ModelInfo
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logPanic("model refresh", recovered)
			}
		}()
		fetched = fetchModelsUncached()
	}()

	modelsCacheMu.Lock()
	modelsRefreshing = false
	if len(fetched) > 0 {
		modelsCache = fetched
		modelsCacheTime = time.Now()
	}
	current := modelsCache
	modelsCacheMu.Unlock()
	if len(current) > 0 {
		return current
	}
	return fallbackModels
}

// fetchModelsUncached does the HTTP work with no lock held, returning nil on failure so
// the caller keeps what it had. The API is newest-first, so glm-4.7 is a break.
func fetchModelsUncached() []ModelInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL+"/api/models", nil)
	if err != nil {
		logError("fetchModels request: " + err.Error())
		return nil
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
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logError(fmt.Sprintf("fetchModels status: %d", resp.StatusCode))
		return nil
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
		return nil
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
		logInfo(fmt.Sprintf("Fetched %d models from Z.AI", len(filtered)))
	}
	return filtered
}

// getModelCapabilities returns a model's raw capabilities map.
func getModelCapabilities(modelID string) map[string]interface{} {
	for _, m := range fetchModelsFromZAI() {
		if strings.EqualFold(m.ID, modelID) {
			return m.Capabilities
		}
	}
	return nil
}

// modelSupportsReasoningEffort needs the capability explicitly true; false and
// missing both mean unsupported.
func modelSupportsReasoningEffort(modelID string) bool {
	modelID = canonicalUpstreamModelID(modelID)
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

// visionModelPreference orders the fallbacks for a request carrying images whose
// model cannot see them: GLM-5v-Turbo is the dedicated vision model, x-preview-l
// (GLM-5.3-Flash) the vision-capable 5.3 variant.
var visionModelPreference = []string{"GLM-5v-Turbo", "x-preview-l"}

func capsHaveVision(caps map[string]interface{}) bool {
	if caps == nil {
		return false
	}
	v, ok := caps["vision"].(bool)
	return ok && v
}

// modelSupportsVision reports whether Z.AI advertises image input.
func modelSupportsVision(modelID string) bool {
	modelID = canonicalUpstreamModelID(modelID)
	if modelID == "" {
		return false
	}
	return capsHaveVision(getModelCapabilities(modelID))
}

func modelAliasKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		}
	}
	return b.String()
}

var modelAliasTokenRe = regexp.MustCompile(`[a-z0-9]+`)

func uniqueLowerTokens(s string) []string {
	raw := modelAliasTokenRe.FindAllString(strings.ToLower(s), -1)
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func compileModelAliasRegex(tokens []string) *regexp.Regexp {
	if len(tokens) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`(?i)\b(?:`)
	for i, t := range tokens {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(regexp.QuoteMeta(t))
	}
	b.WriteString(`)\b`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func resolveModelAlias(modelID string, models []ModelInfo) string {
	if modelID == "" {
		return ""
	}
	for _, m := range models {
		if strings.EqualFold(m.ID, modelID) {
			return m.ID
		}
	}
	key := modelAliasKey(modelID)
	if key == "" {
		return ""
	}
	for _, m := range models {
		if modelAliasKey(m.ID) == key || modelAliasKey(m.Name) == key {
			return m.ID
		}
	}
	tokens := uniqueLowerTokens(modelID)
	re := compileModelAliasRegex(tokens)
	if re == nil {
		return ""
	}
	bestID := ""
	bestMatches := 0
	bestDistance := 0
	for _, m := range models {
		candidate := strings.ToLower(strings.TrimSpace(m.ID + " " + m.Name))
		hits := re.FindAllString(candidate, -1)
		if len(hits) == 0 {
			continue
		}
		seenHits := make(map[string]struct{}, len(hits))
		for _, h := range hits {
			seenHits[strings.ToLower(h)] = struct{}{}
		}
		matchCount := len(seenHits)
		nameKey := modelAliasKey(m.Name)
		if nameKey == "" {
			nameKey = modelAliasKey(m.ID)
		}
		distance := absInt(len(nameKey) - len(key))
		if bestID == "" || matchCount > bestMatches || (matchCount == bestMatches && distance < bestDistance) {
			bestMatches = matchCount
			bestDistance = distance
			bestID = m.ID
		}
	}
	return bestID
}

func canonicalUpstreamModelID(modelID string) string {
	models := fetchModelsFromZAI()
	if resolved := resolveModelAlias(modelID, models); resolved != "" {
		return resolved
	}
	if modelAliasKey(modelID) == modelAliasKey("glm-5.3-flash") {
		if resolved := resolveModelAlias("x-preview-l", models); resolved != "" {
			return resolved
		}
		return "x-preview-l"
	}
	return modelID
}

// resolveVisionModel returns a model that can actually see images. Text-only
// models answer image requests with a long stall and then INTERNAL_ERROR, so
// those get redirected. "" means none is available; send the request as-is.
func resolveVisionModel(requested string) string {
	if modelSupportsVision(requested) {
		return requested
	}
	models := fetchModelsFromZAI()
	for _, want := range visionModelPreference {
		for _, m := range models {
			if strings.EqualFold(m.ID, want) && capsHaveVision(m.Capabilities) {
				return m.ID
			}
		}
	}
	for _, m := range models {
		if capsHaveVision(m.Capabilities) {
			return m.ID
		}
	}
	return ""
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

func architectureFor(caps map[string]interface{}) map[string]interface{} {
	inputModalities := []string{"text"}
	modality := "text->text"
	if capsHaveVision(caps) {
		inputModalities = []string{"text", "image"}
		modality = "text+image->text"
	}
	return map[string]interface{}{
		"modality":          modality,
		"input_modalities":  inputModalities,
		"output_modalities": []string{"text"},
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
			"architecture": architectureFor(m.Capabilities),
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
