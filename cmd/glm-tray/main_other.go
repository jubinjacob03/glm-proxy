//go:build !windows

// The tray is Windows-only; this stub keeps build and vet working elsewhere.
package main

import "fmt"

func main() {
	fmt.Println("glm-tray is a Windows-only application.")
}
