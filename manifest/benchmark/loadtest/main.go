// loadtest measures TTFT and total latency against the SSE benchmark
// upstream through the higress gateway. Goroutine-per-worker, keep-alive,
// stdlib only.
//
// The API key sent to the apikey-extract plugin comes from $PERF_API_KEY
// (a throwaway benchmark value; a random one is generated when unset — no
// credentials live in this file).
//
// Usage: loadtest -host 127.0.0.1 -port 32356 -concurrency 16 -seconds 30
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type result struct {
	TTFT  float64 // ms to the first SSE body byte
	Total float64 // ms to end of stream
	Err   bool
}

func main() {
	host := flag.String("host", "127.0.0.1", "gateway host")
	port := flag.Int("port", 32356, "gateway nodeport")
	hostHeader := flag.String("host-header", "perf.test", "Host header routed by the gateway")
	concurrency := flag.Int("concurrency", 16, "worker goroutines")
	warmup := flag.Float64("warmup", 5.0, "warmup seconds (discarded)")
	seconds := flag.Float64("seconds", 30.0, "measured seconds")
	flag.Parse()

	apiKey := os.Getenv("PERF_API_KEY")
	if apiKey == "" {
		apiKey = fmt.Sprintf("perf-%08x", rand.Uint64())
	}

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			IdleConnTimeout:     120 * time.Second,
		},
	}
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"perf_chunks":20,"perf_interval_ms":10}`

	run := func(seconds float64) []result {
		results := make([]result, 0, 4096)
		var mu sync.Mutex
		stop := time.Now().Add(time.Duration(seconds * float64(time.Second)))
		var wg sync.WaitGroup
		for i := 0; i < *concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(stop) {
					r := one(client, *host, *port, *hostHeader, apiKey, body)
					mu.Lock()
					results = append(results, r)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return results
	}

	run(*warmup) // warmup, discarded
	results := run(*seconds)

	var oks []result
	errs := 0
	for _, r := range results {
		if r.Err {
			errs++
		} else {
			oks = append(oks, r)
		}
	}

	ms := func(vs []float64) map[string]float64 {
		sort.Float64s(vs)
		pick := func(p float64) float64 {
			if len(vs) == 0 {
				return -1
			}
			idx := int(p / 100 * float64(len(vs)-1))
			return round2(vs[idx])
		}
		var sum float64
		for _, v := range vs {
			sum += v
		}
		return map[string]float64{
			"mean": round2(sum / float64(len(vs))),
			"p50":  pick(50), "p90": pick(90), "p99": pick(99),
		}
	}

	ttft := make([]float64, 0, len(oks))
	total := make([]float64, 0, len(oks))
	for _, r := range oks {
		ttft = append(ttft, r.TTFT)
		total = append(total, r.Total)
	}

	wall := *seconds
	out := map[string]any{
		"requests": len(oks),
		"errors":   errs,
		"rps":      round2(float64(len(oks)) / wall),
		"ttft_ms":  ms(ttft),
		"total_ms": ms(total),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func one(client *http.Client, host string, port int, hostHeader, apiKey, body string) result {
	start := time.Now()
	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s:%d/chat/completions", host, port), strings.NewReader(body))
	if err != nil {
		return result{Err: true}
	}
	req.Host = hostHeader
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return result{Err: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return result{Err: true}
	}
	var first time.Time
	buf := make([]byte, 256)
	for {
		n, err := resp.Body.Read(buf)
		if first.IsZero() && n > 0 {
			first = time.Now()
		}
		if err != nil {
			break
		}
	}
	if first.IsZero() {
		return result{Err: true}
	}
	end := time.Now()
	return result{TTFT: float64(first.Sub(start)) / 1e6, Total: float64(end.Sub(start)) / 1e6}
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
