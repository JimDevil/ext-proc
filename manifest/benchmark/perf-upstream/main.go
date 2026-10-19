// perf-upstream is the streaming upstream for the ext-proc benchmark: it
// mimics an LLM streaming completion — response headers and the FIRST data
// chunk go out immediately (TTFT sensitivity), the rest arrive on a fixed
// interval, then the stream ends.
//
//	POST /chat/completions   body: {"model":"...", "perf_chunks":20, "perf_interval_ms":10}
//	GET  /                   health + request-header echo
//
// The headers the plugin chain injects (x-higress-llm-model,
// x-extracted-apikey) are echoed back as x-echo-* response headers, giving
// every test run positive evidence of what the plugins actually did.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

type perfRequest struct {
	Model          string `json:"model"`
	PerfChunks     int    `json:"perf_chunks"`
	PerfIntervalMs int    `json:"perf_interval_ms"`
}

func handleCompletions(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req perfRequest
	_ = json.Unmarshal(body, &req)
	if req.PerfChunks <= 0 {
		req.PerfChunks = 20
	}
	if req.PerfIntervalMs <= 0 {
		req.PerfIntervalMs = 10
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("x-upstream-first-byte", strconv.FormatInt(time.Now().UnixNano()/1e6, 10))
	// Echo back the headers the plugins inject, as positive e2e evidence.
	for _, name := range []string{"x-higress-llm-model", "x-extracted-apikey", "x-model-chat"} {
		if v := r.Header.Get(name); v != "" {
			w.Header().Set("x-echo-"+name, v)
		}
	}
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}

	// First chunk immediately — TTFT is measured against this byte.
	fmt.Fprintf(w, "data: {\"chunk\":0,\"model\":%q}\n\n", req.Model)
	flusher.Flush()

	for i := 1; i < req.PerfChunks; i++ {
		time.Sleep(time.Duration(req.PerfIntervalMs) * time.Millisecond)
		fmt.Fprintf(w, "data: {\"chunk\":%d}\n\n", i)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	for k, vs := range r.Header {
		for _, v := range vs {
			fmt.Fprintf(w, "%s: %s\n", k, v)
		}
	}
}

func main() {
	port := flag.Int("port", 8080, "listen port")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", handleCompletions)
	mux.HandleFunc("/", handleHealth)
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("perf-upstream listening on :%d", *port)
	log.Fatal(srv.ListenAndServe())
}
