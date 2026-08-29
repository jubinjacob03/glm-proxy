// GLM-Free-API — an OpenAI- and Anthropic-compatible proxy for Z.AI.
//
// Thin entry point; the bridge lives in internal/zbridge and token collection is
// a separate binary under cmd/token-collector.
//
//	go build -trimpath -ldflags="-s -w" -o zai-api .

package main

import "zai-api/internal/zbridge"

func main() {
	zbridge.Run()
}
