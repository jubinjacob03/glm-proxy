package zbridge

import (
	"strings"
)

func normalizeFeatureKey(k string) string {
	var sb strings.Builder
	for i, r := range k {
		if i > 0 && r >= 'A' && r <= 'Z' {
			sb.WriteByte('_')
		}
		if r >= 'A' && r <= 'Z' {
			sb.WriteRune(r + 32)
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// getModelFeatureState returns the per-model state, creating it if necessary.
func getModelFeatureState(modelID string) *ModelFeatureState {
	modelFeatureStatesMu.Lock()
	defer modelFeatureStatesMu.Unlock()
	if s, ok := modelFeatureStates[modelID]; ok {
		return s
	}
	s := &ModelFeatureState{
		IncludeAll: false,
		Overrides:  make(map[string]interface{}),
	}
	modelFeatureStates[modelID] = s
	return s
}

// resolveFeaturesForModel computes the final feature map for /completions.
func resolveFeaturesForModel(modelID string) map[string]interface{} {
	caps := getModelCapabilities(modelID)
	modelFeatureStatesMu.Lock()
	state, ok := modelFeatureStates[modelID]
	modelFeatureStatesMu.Unlock()
	if !ok {
		state = &ModelFeatureState{
			IncludeAll: false,
			Overrides:  make(map[string]interface{}),
		}
	}
	return resolveFeaturesWithState(caps, state)
}

// resolveFeaturesWithState resolves caps against per-model state:
//   - web_search and auto_web_search are off unless requested.
//   - enable_thinking defaults to true; 'think' never reaches the request.
//   - stored overrides take precedence over server capabilities.
//   - image_generation is always forced off on this endpoint.
func resolveFeaturesWithState(caps map[string]interface{}, state *ModelFeatureState) map[string]interface{} {
	result := make(map[string]interface{}, len(caps)+len(state.Overrides)+2)

	if state.IncludeAll {
		for k, v := range caps {
			// In capabilities this is a boolean support flag, not a value;
			// sendToZAI resolves the real per-request setting.
			if k == "reasoning_effort" {
				continue
			}
			result[k] = v
		}
	}

	for k, v := range state.Overrides {
		result[k] = v
	}

	delete(result, "reasoning_effort")

	if _, ok := result["enable_thinking"]; !ok {
		result["enable_thinking"] = true
	}
	delete(result, "think")
	result["image_generation"] = false

	return result
}
