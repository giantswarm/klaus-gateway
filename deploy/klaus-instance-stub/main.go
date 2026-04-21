// klaus-instance-stub mimics the HTTP surface of a real klaus instance just
// enough for the compose smoke harness to pass:
//
//	POST /v1/chat/completions -- returns a short SSE stream
//	POST /mcp                 -- responds to JSON-RPC `tools/call` for the
//	                             `messages` tool with a minimal transcript
//	GET  /status              -- 200 ok
//
// The stub is not a product surface. It exists solely so CI can verify
// klaus-gateway end-to-end without pulling the real klaus image (which is
// heavy and takes an LLM API key to be useful).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := os.Getenv("STUB_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	})
	mux.HandleFunc("/v1/chat/completions", handleCompletions)
	mux.HandleFunc("/mcp", handleMCP)

	log.Printf("klaus-instance-stub listening on %s", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	chunks := []string{"hello", " from", " klaus-instance-stub"}
	for _, c := range chunks {
		payload := map[string]any{
			"choices": []map[string]any{{
				"delta": map[string]string{"content": c},
				"index": 0,
			}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result := map[string]any{
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello from stub"},
		},
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(req.ID),
		"result":  result,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envelope)
}
