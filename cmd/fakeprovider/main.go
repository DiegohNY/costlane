// Command fakeprovider runs a stand-in LLM provider.
//
// It speaks the OpenAI, Anthropic and Gemini dialects, and every aspect of a
// response — token counts, latency, failures, malformed chunks — is chosen by
// request headers. It exists so that the first command in the README works
// without a credential and without spending anything.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/DiegohNY/costlane/internal/fakeprovider"
)

func main() {
	addr := os.Getenv("FAKEPROVIDER_ADDR")
	if addr == "" {
		addr = ":8081"
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           fakeprovider.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Printf("fakeprovider: listening on %s\n", addr)
	fmt.Println("fakeprovider: POST /v1/chat/completions, /v1/messages, /v1beta/models/{model}:generateContent")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "fakeprovider: %v\n", err)
		os.Exit(1)
	}
}
