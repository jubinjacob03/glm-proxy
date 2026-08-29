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

// The live *ModelFeatureState never leaves the mutex; see snapshotModelFeatureState.

// copyFeatureStateLocked duplicates a state; the caller must hold the mutex.
func copyFeatureStateLocked(s *ModelFeatureState) *ModelFeatureState {
	out := &ModelFeatureState{
		IncludeAll: s.IncludeAll,
		Overrides:  make(map[string]interface{}, len(s.Overrides)),
	}
	for k, v := range s.Overrides {
		out.Overrides[k] = v
	}
	return out
}

// snapshotModelFeatureState copies the state for reading outside the lock: aliasing it
// let a reader range Overrides while POST /features wrote it, a fatal map race. Creates
// nothing, so a read never registers a model.
func snapshotModelFeatureState(modelID string) *ModelFeatureState {
	modelFeatureStatesMu.Lock()
	defer modelFeatureStatesMu.Unlock()
	if s, ok := modelFeatureStates[modelID]; ok {
		return copyFeatureStateLocked(s)
	}
	return &ModelFeatureState{Overrides: make(map[string]interface{})}
}

// resolveFeaturesForModel builds the feature map sent to /completions.
func resolveFeaturesForModel(modelID string) map[string]interface{} {
	caps := getModelCapabilities(modelID)
	return resolveFeaturesWithState(caps, snapshotModelFeatureState(modelID))
}

// resolveFeaturesWithState resolves capabilities against per-model state:
//   - web_search and auto_web_search are off unless requested.
//   - enable_thinking defaults to true; 'think' never reaches the request.
//   - stored overrides take precedence over server capabilities.
//   - image_generation is always forced off on this endpoint.
func resolveFeaturesWithState(caps map[string]interface{}, state *ModelFeatureState) map[string]interface{} {
	result := make(map[string]interface{}, len(caps)+len(state.Overrides)+2)

	if state.IncludeAll {
		for k, v := range caps {
			// Here it is a boolean support flag, not a value; sendToZAI resolves
			// the real per-request setting.
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
