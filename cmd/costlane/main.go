// Command costlane runs the gateway.
package main

import (
	"fmt"
	"os"

	"github.com/DiegohNY/costlane/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "costlane: %v\n", err)
		os.Exit(1)
	}
	// Serving arrives in F5; F0 proves the binary builds and validates.
	fmt.Printf("costlane: configuration valid: %s\n", cfg)
}
