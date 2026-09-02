package zbridge

import (
	"testing"
	"time"
)

func TestCanonicalUpstreamModelID(t *testing.T) {
	origCache := modelsCache
	origTime := modelsCacheTime
	origRefreshing := modelsRefreshing
	modelsCacheMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "abc", Name: "glm-5.3-flash"},
		{ID: "x-preview-l", Name: "GLM-5.3-Flash"},
		{ID: "x-preview-z", Name: "GLM-6-Flash"},
		{ID: "model-ultra-01", Name: "GLM 6 Ultra Reasoning"},
		{ID: "glm-5.3", Name: "GLM-5.3"},
	}
	modelsCacheTime = time.Now()
	modelsRefreshing = false
	modelsCacheMu.Unlock()
	defer func() {
		modelsCacheMu.Lock()
		modelsCache = origCache
		modelsCacheTime = origTime
		modelsRefreshing = origRefreshing
		modelsCacheMu.Unlock()
	}()

	tests := []struct {
		in   string
		want string
	}{
		{in: "glm-flash-5.3", want: "abc"},
		{in: "glm-5.3-flash", want: "abc"},
		{in: "GLM-5.3-FLASH", want: "abc"},
		{in: "x-preview-l", want: "x-preview-l"},
		{in: "glm 6 flash", want: "x-preview-z"},
		{in: "GLM_6_FLASH", want: "x-preview-z"},
		{in: "GLM 6 Ultra", want: "model-ultra-01"},
		{in: "glm-5.3", want: "glm-5.3"},
		{in: "", want: ""},
	}

	for _, tc := range tests {
		if got := canonicalUpstreamModelID(tc.in); got != tc.want {
			t.Fatalf("canonicalUpstreamModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestModelSupportsVisionUsesCanonicalAlias(t *testing.T) {
	origCache := modelsCache
	origTime := modelsCacheTime
	origRefreshing := modelsRefreshing
	modelsCacheMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "x-preview-l", Name: "GLM-5.3-Flash", Capabilities: map[string]interface{}{"vision": true}},
	}
	modelsCacheTime = time.Now()
	modelsRefreshing = false
	modelsCacheMu.Unlock()
	defer func() {
		modelsCacheMu.Lock()
		modelsCache = origCache
		modelsCacheTime = origTime
		modelsRefreshing = origRefreshing
		modelsCacheMu.Unlock()
	}()

	if !modelSupportsVision("glm-5.3-flash") {
		t.Fatal("expected glm-5.3-flash to resolve via x-preview-l capability")
	}
}

func TestResolveModelAliasRegexClosestMatch(t *testing.T) {
	models := []ModelInfo{
		{ID: "m-a", Name: "GLM 7 Fast Pro"},
		{ID: "m-b", Name: "GLM 7 Fast"},
	}
	if got := resolveModelAlias("glm-fast-7", models); got != "m-b" {
		t.Fatalf("resolveModelAlias closest = %q, want m-b", got)
	}
}
