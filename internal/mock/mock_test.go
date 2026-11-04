package mock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

func fastConfig() Config {
	cfg := DefaultConfig()
	cfg.LatencyMS = 0
	cfg.ChunkDelayMS = 0
	return cfg
}

func start(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// expectedBill recomputes the charge independently of the server's ledger.
func expectedBill(cfg Config, input, output int) money.Pico {
	return money.Pico(input)*cfg.InputRate + money.Pico(output)*cfg.OutputRate
}

const messages = `[{"role":"user","content":"hello"}]`

func TestNewRejectsBadConfig(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.InputRate = -1 },
		func(c *Config) { c.OutputRate = -1 },
		func(c *Config) { c.OutputTokens = 0 },
		func(c *Config) { c.OutputTokens = 1_000_001 },
		func(c *Config) { c.Chunks = 0 },
		func(c *Config) { c.LatencyMS = -1 },
		func(c *Config) { c.ChunkDelayMS = -1 },
		func(c *Config) { c.OnDisconnect = "maybe" },
		func(c *Config) { c.Usage = "maybe" },
	}
	for i, mutate := range bad {
		cfg := DefaultConfig()
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: invalid config accepted", i)
		}
	}
}

func TestUnaryUsageMatchesBill(t *testing.T) {
	tests := []struct {
		name, path, body, inField, outField string
		parts                               []string
	}{
		{
			name: "openai", path: "/v1/chat/completions",
			body:    `{"model":"m","messages":` + messages + `,"max_tokens":100}`,
			inField: "prompt_tokens", outField: "completion_tokens",
			parts: []string{messages},
		},
		{
			name: "openai without v1", path: "/chat/completions",
			body:    `{"model":"m","messages":` + messages + `,"max_completion_tokens":100}`,
			inField: "prompt_tokens", outField: "completion_tokens",
			parts: []string{messages},
		},
		{
			name: "anthropic", path: "/v1/messages",
			body:    `{"model":"m","system":"be brief","messages":` + messages + `,"max_tokens":100}`,
			inField: "input_tokens", outField: "output_tokens",
			parts: []string{messages, `"be brief"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fastConfig()
			s, ts := start(t, cfg)
			status, raw := post(t, ts.URL+tt.path, tt.body)
			if status != http.StatusOK {
				t.Fatalf("status %d: %s", status, raw)
			}
			var resp struct {
				Usage map[string]int `json:"usage"`
			}
			if err := json.Unmarshal([]byte(raw), &resp); err != nil {
				t.Fatal(err)
			}
			parts := make([][]byte, len(tt.parts))
			for i, p := range tt.parts {
				parts[i] = []byte(p)
			}
			wantInput := InputTokens(parts...)
			if resp.Usage[tt.inField] != wantInput || resp.Usage[tt.outField] != 100 {
				t.Fatalf("usage = %v, want input %d output 100", resp.Usage, wantInput)
			}
			got := s.Snapshot()
			if want := expectedBill(cfg, wantInput, 100); got.Billed != want {
				t.Fatalf("billed %s, want %s", got.Billed, want)
			}
			if got.Requests != 1 || got.InFlight != 0 {
				t.Fatalf("requests=%d in_flight=%d", got.Requests, got.InFlight)
			}
		})
	}
}

func TestInvalidRequestsAreNotBilled(t *testing.T) {
	tests := []struct {
		name, path, body string
		status           int
	}{
		{"bad json", "/v1/chat/completions", `{`, 400},
		{"wrong type", "/v1/chat/completions", `{"model":1,"messages":[{}]}`, 400},
		{"no model", "/v1/chat/completions", `{"messages":` + messages + `}`, 400},
		{"null messages", "/v1/chat/completions", `{"model":"m","messages":null}`, 400},
		{"empty messages", "/v1/chat/completions", `{"model":"m","messages":[ ]}`, 400},
		{"object messages", "/v1/chat/completions", `{"model":"m","messages":{"a":1}}`, 400},
		{"zero limit", "/v1/chat/completions", `{"model":"m","messages":` + messages + `,"max_tokens":0}`, 400},
		{"negative limit", "/v1/chat/completions", `{"model":"m","messages":` + messages + `,"max_completion_tokens":-1}`, 400},
		{"n above one", "/v1/chat/completions", `{"model":"m","messages":` + messages + `,"n":2}`, 400},
		{"anthropic needs limit", "/v1/messages", `{"model":"m","messages":` + messages + `}`, 400},
		{"anthropic no messages", "/v1/messages", `{"model":"m","max_tokens":5}`, 400},
		{"anthropic bad json", "/v1/messages", `[`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ts := start(t, fastConfig())
			if status, raw := post(t, ts.URL+tt.path, tt.body); status != tt.status {
				t.Fatalf("status %d, want %d: %.200s", status, tt.status, raw)
			}
			got := s.Snapshot()
			if got.Billed != 0 || got.Requests != 0 || got.Rejected != 1 || got.InFlight != 0 {
				t.Fatalf("bad call changed the ledger: %+v", got)
			}
		})
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	s, err := New(fastConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Served directly: over a socket the early close can race the client's upload.
	body := `{"model":"m","messages":["` + strings.Repeat("a", maxBody) + `"]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
	if got := s.Snapshot(); got.Billed != 0 || got.Rejected != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestStreamUsageRules(t *testing.T) {
	cfg := fastConfig()
	s, ts := start(t, cfg)

	// Without include_usage the stream carries no usage, as on the real API,
	// yet the oracle must still bill it.
	_, raw := post(t, ts.URL+"/v1/chat/completions", `{"model":"m","stream":true,"messages":`+messages+`}`)
	if strings.Contains(raw, "completion_tokens") {
		t.Fatalf("usage sent without include_usage:\n%s", raw)
	}
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("stream did not end with [DONE]:\n%s", raw)
	}
	if got := s.Snapshot(); got.OutputTokens != int64(cfg.OutputTokens) {
		t.Fatalf("output tokens %d, want %d", got.OutputTokens, cfg.OutputTokens)
	}

	_, raw = post(t, ts.URL+"/v1/chat/completions", `{"model":"m","stream":true,"stream_options":{"include_usage":true},"max_tokens":7,"messages":`+messages+`}`)
	if !strings.Contains(raw, `"completion_tokens":7`) || !strings.Contains(raw, `"finish_reason":"length"`) {
		t.Fatalf("expected usage and length finish:\n%s", raw)
	}

	_, raw = post(t, ts.URL+"/v1/messages", `{"model":"m","stream":true,"max_tokens":7,"messages":`+messages+`}`)
	for _, want := range []string{"event: message_start", "event: content_block_delta", `"output_tokens":7`, `"stop_reason":"max_tokens"`, "event: message_stop"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in:\n%s", want, raw)
		}
	}
}

func TestChunksAddUpWhenTheyDoNotDivideEvenly(t *testing.T) {
	cfg := fastConfig()
	cfg.OutputTokens = 7
	cfg.Chunks = 3
	s, ts := start(t, cfg)
	post(t, ts.URL+"/v1/chat/completions", `{"model":"m","stream":true,"messages":`+messages+`}`)
	if got := s.Snapshot().OutputTokens; got != 7 {
		t.Fatalf("output tokens %d, want 7", got)
	}
}

func TestMoreChunksThanTokens(t *testing.T) {
	cfg := fastConfig()
	cfg.OutputTokens = 2
	cfg.Chunks = 5
	s, ts := start(t, cfg)
	post(t, ts.URL+"/v1/chat/completions", `{"model":"m","stream":true,"messages":`+messages+`}`)
	if got := s.Snapshot().OutputTokens; got != 2 {
		t.Fatalf("output tokens %d, want 2", got)
	}
}

func TestUsageFaultsStillBillTheTruth(t *testing.T) {
	for _, mode := range []string{"omit", "malformed"} {
		for _, tt := range []struct{ path, body, leak string }{
			{"/v1/chat/completions", `{"model":"m","messages":` + messages + `}`, `"completion_tokens":500`},
			{"/v1/chat/completions", `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":` + messages + `}`, `"completion_tokens":500`},
			{"/v1/messages", `{"model":"m","max_tokens":500,"messages":` + messages + `}`, `"output_tokens":500`},
			{"/v1/messages", `{"model":"m","max_tokens":500,"stream":true,"messages":` + messages + `}`, `"output_tokens":500`},
		} {
			cfg := fastConfig()
			cfg.Usage = mode
			s, ts := start(t, cfg)
			status, raw := post(t, ts.URL+tt.path, tt.body)
			if status != http.StatusOK {
				t.Fatalf("%s %s: status %d", mode, tt.path, status)
			}
			if strings.Contains(raw, tt.leak) {
				t.Fatalf("%s %s leaked true usage:\n%s", mode, tt.path, raw)
			}
			if got := s.Snapshot(); got.OutputTokens != 500 || got.Billed == 0 {
				t.Fatalf("%s %s: oracle should bill the truth, got %+v", mode, tt.path, got)
			}
		}
	}
}

func TestIgnoreMaxTokensBillsPastTheLimit(t *testing.T) {
	cfg := fastConfig()
	cfg.IgnoreMaxTokens = true
	s, ts := start(t, cfg)
	post(t, ts.URL+"/v1/chat/completions", `{"model":"m","max_tokens":1,"messages":`+messages+`}`)
	if got := s.Snapshot().OutputTokens; got != int64(cfg.OutputTokens) {
		t.Fatalf("output tokens %d, want %d", got, cfg.OutputTokens)
	}
}

// cancelAfterFirstByte opens a stream, reads one byte, then drops the connection.
func cancelAfterFirstByte(t *testing.T, url, body string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := resp.Body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
}

func waitIdle(t *testing.T, s *Server) Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.Snapshot(); got.InFlight == 0 {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("calls never drained")
	return Snapshot{}
}

func TestDisconnectBilling(t *testing.T) {
	stream := `{"model":"m","stream":true,"messages":` + messages + `}`
	for _, tt := range []struct {
		mode string
		full bool
	}{{"full", true}, {"delivered", false}} {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := fastConfig()
			cfg.OnDisconnect = tt.mode
			cfg.Chunks = 20
			cfg.ChunkDelayMS = 20
			s, ts := start(t, cfg)
			cancelAfterFirstByte(t, ts.URL+"/v1/chat/completions", stream)
			got := waitIdle(t, s)
			if got.Disconnects != 1 {
				t.Fatalf("disconnects = %d, want 1", got.Disconnects)
			}
			if tt.full && got.OutputTokens != int64(cfg.OutputTokens) {
				t.Fatalf("full mode billed %d tokens, want %d", got.OutputTokens, cfg.OutputTokens)
			}
			if !tt.full && (got.OutputTokens == 0 || got.OutputTokens >= int64(cfg.OutputTokens)) {
				t.Fatalf("delivered mode billed %d tokens, want a partial amount", got.OutputTokens)
			}
		})
	}
}

func TestDisconnectDuringLatency(t *testing.T) {
	for _, tt := range []struct {
		mode string
		want int64
	}{{"full", 500}, {"delivered", 0}} {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := fastConfig()
			cfg.OnDisconnect = tt.mode
			cfg.LatencyMS = 500
			s, ts := start(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":`+messages+`}`))
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				t.Fatal("expected the client timeout to fire first")
			}
			got := waitIdle(t, s)
			if got.Requests != 1 || got.OutputTokens != tt.want || got.InputTokens == 0 {
				t.Fatalf("%+v, want %d output tokens", got, tt.want)
			}
		})
	}
}

func TestResetRules(t *testing.T) {
	cfg := fastConfig()
	cfg.LatencyMS = 200
	s, ts := start(t, cfg)

	if status, _ := post(t, ts.URL+"/_oracle/reset", `{"usage":"nope"}`); status != http.StatusBadRequest {
		t.Fatalf("invalid override got status %d", status)
	}
	if status, _ := post(t, ts.URL+"/_oracle/reset", `{"typo_field":1}`); status != http.StatusBadRequest {
		t.Fatalf("unknown field got status %d", status)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":`+messages+`}`))
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.Snapshot().InFlight == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the call never reached the provider")
		}
		time.Sleep(time.Millisecond)
	}
	if status, _ := post(t, ts.URL+"/_oracle/reset", ""); status != http.StatusConflict {
		t.Fatalf("reset during a call got status %d, want 409", status)
	}
	wg.Wait()
	if s.Snapshot().Billed == 0 {
		t.Fatal("the refused reset cleared the ledger")
	}

	if status, _ := post(t, ts.URL+"/_oracle/reset", `{"usage":"omit","latency_ms":0}`); status != http.StatusOK {
		t.Fatalf("reset got status %d", status)
	}
	got := s.Snapshot()
	if got.Billed != 0 || got.Requests != 0 || got.Config.Usage != "omit" {
		t.Fatalf("reset did not apply: %+v", got)
	}
	// An override must not outlive its run.
	if status, _ := post(t, ts.URL+"/_oracle/reset", ""); status != http.StatusOK {
		t.Fatalf("reset got status %d", status)
	}
	if got := s.Snapshot().Config; got.Usage != "normal" || got.LatencyMS != 200 {
		t.Fatalf("override leaked into the next run: %+v", got)
	}
}

func TestUnroutedCallsAreRecorded(t *testing.T) {
	s, ts := start(t, fastConfig())
	if status, _ := post(t, ts.URL+"/v1/responses", `{}`); status != http.StatusNotFound {
		t.Fatalf("status %d", status)
	}
	got := s.Snapshot()
	if got.Unrouted != 1 || got.LastUnrouted != "POST /v1/responses" || got.Billed != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestConcurrentCallsBillExactly(t *testing.T) {
	cfg := fastConfig()
	cfg.LatencyMS = 20
	s, ts := start(t, cfg)
	const calls = 64
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"m","messages":`+messages+`}`))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
	}
	wg.Wait()
	got := waitIdle(t, s)
	want := money.Pico(calls) * expectedBill(cfg, InputTokens([]byte(messages)), cfg.OutputTokens)
	if got.Billed != want || got.Requests != calls {
		t.Fatalf("billed %s over %d calls, want %s over %d", got.Billed, got.Requests, want, calls)
	}
	if got.MaxInFlight < 2 {
		t.Fatalf("max in flight %d: the calls never overlapped", got.MaxInFlight)
	}
}
