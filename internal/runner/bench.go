package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// BenchOptions describes one latency measurement.
type BenchOptions struct {
	Target      string
	Format      string
	Model       string
	APIKey      string
	Rate        int // calls per second
	Duration    time.Duration
	PromptBytes int
	MaxTokens   int
	Timeout     time.Duration
}

// BenchReport holds latency percentiles in milliseconds.
type BenchReport struct {
	Target   string  `json:"target"`
	Rate     int     `json:"rate_per_second"`
	Sent     int     `json:"sent"`
	Failed   int     `json:"failed"`
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
	P99MS    float64 `json:"p99_ms"`
	P999MS   float64 `json:"p999_ms"`
	MaxMS    float64 `json:"max_ms"`
	Achieved float64 `json:"achieved_per_second"`
}

// Bench sends calls on a fixed schedule whatever the target's speed, and times each
// from when it was due. Timing from the actual send would hide queueing, because a
// slow target would simply delay the sends that would have measured it being slow.
func Bench(ctx context.Context, o BenchOptions) (*BenchReport, error) {
	if o.Rate < 1 || o.Duration <= 0 {
		return nil, errors.New("rate and duration must be above zero")
	}
	if o.Format == "" {
		o.Format = "openai"
	}
	if o.Model == "" {
		o.Model = "gpt-4o-mini"
	}
	if o.PromptBytes == 0 {
		o.PromptBytes = 1000
	}
	if o.MaxTokens == 0 {
		o.MaxTokens = 100
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	url := endpoint(o.Target, o.Format)
	headers := buildHeaders(Options{Format: o.Format, APIKey: o.APIKey})
	raw, err := json.Marshal(map[string]any{
		"model": o.Model, "max_tokens": o.MaxTokens,
		"messages": []map[string]string{{"role": "user", "content": strings.Repeat("word ", o.PromptBytes/5)}},
	})
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 1024, DisableCompression: true},
		Timeout:   o.Timeout,
	}
	defer client.CloseIdleConnections()

	total := int(o.Duration.Seconds() * float64(o.Rate))
	interval := time.Second / time.Duration(o.Rate)
	latencies := make([]time.Duration, 0, total)
	failed := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := time.Now()
	for i := range total {
		due := start.Add(time.Duration(i) * interval)
		if wait := time.Until(due); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := benchCall(ctx, client, url, headers, raw)
			took := time.Since(due)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				latencies = append(latencies, took)
			} else {
				failed++
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if len(latencies) == 0 {
		return nil, fmt.Errorf("all %d calls failed", total)
	}
	slices.Sort(latencies)
	at := func(q float64) float64 {
		return float64(latencies[min(int(q*float64(len(latencies))), len(latencies)-1)].Microseconds()) / 1000
	}
	return &BenchReport{
		Target: o.Target, Rate: o.Rate, Sent: total, Failed: failed,
		P50MS: at(0.50), P95MS: at(0.95), P99MS: at(0.99), P999MS: at(0.999), MaxMS: at(1),
		Achieved: float64(len(latencies)) / elapsed.Seconds(),
	}, nil
}

func benchCall(ctx context.Context, client *http.Client, url string, headers http.Header, body []byte) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
