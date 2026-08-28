//go:build !windows

// The tray supervisor is Windows-only. This stub keeps `go build ./...` and
// `go vet ./...` working on other platforms.
package main

import "fmt"

func main() {
	fmt.Println("glm-tray is a Windows-only application.")
}
