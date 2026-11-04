package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

func startMock(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := mock.DefaultConfig()
	cfg.LatencyMS = 20
	cfg.ChunkDelayMS = 1
	s, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestJudge(t *testing.T) {
	const cap = money.Pico(1000)
	provider := func(requests int64) mock.Snapshot {
		return mock.Snapshot{Requests: requests, MaxInFlight: 5, Disconnects: 1}
	}
	tests := []struct {
		name string
		rep  Report
		want string
	}{
		{"over by one picodollar", Report{Billed: cap + 1, Statuses: map[string]int{"200": 10}, Admitted: 10, Provider: provider(10)}, Breached},
		{"over even with errors", Report{Billed: cap * 3, Statuses: map[string]int{"500": 2, "401": 2, "200": 3}, Admitted: 3, Provider: provider(3)}, Breached},
		{"exactly at the cap", Report{Billed: cap, Statuses: map[string]int{"200": 10, "429": 5}, Admitted: 10, Provider: provider(10)}, Held},
		{"402 refusal", Report{Billed: cap - 1, Statuses: map[string]int{"200": 9, "402": 5}, Admitted: 9, Provider: provider(9)}, Held},
		{"budget error as 400", Report{Billed: cap - 1, Statuses: map[string]int{"200": 9, "400": 5}, Admitted: 9, Provider: provider(9)}, Held},
		{"nothing reached provider", Report{Statuses: map[string]int{"429": 30}, Provider: provider(0)}, Inconclusive},
		{"auth failures mixed in", Report{Billed: 10, Statuses: map[string]int{"200": 1, "401": 29}, Admitted: 1, Provider: provider(1)}, Inconclusive},
		{"route failures mixed in", Report{Billed: 10, Statuses: map[string]int{"200": 1, "404": 2, "429": 2}, Admitted: 1, Provider: provider(1)}, Inconclusive},
		{"redirects", Report{Billed: 10, Statuses: map[string]int{"200": 1, "307": 2}, Admitted: 1, Provider: provider(1)}, Inconclusive},
		{"gateway crashed under load", Report{Billed: 10, Statuses: map[string]int{"200": 1, "503": 29}, Admitted: 1, Provider: provider(1)}, Inconclusive},
		{"connections failed", Report{Billed: 10, Statuses: map[string]int{"200": 1}, TransportErrors: 9, Admitted: 1, Provider: provider(1)}, Inconclusive},
		{"timeouts are not budget decisions", Report{Billed: 10, Statuses: map[string]int{"200": 1, "408": 2}, Admitted: 1, Provider: provider(1)}, Inconclusive},
		{"never refused and under cap", Report{Billed: 10, Statuses: map[string]int{"200": 30}, Admitted: 30, Provider: provider(30)}, Inconclusive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.rep.Cap = cap
			got, reasons, _ := judge(&tt.rep, Burst)
			if got != tt.want {
				t.Fatalf("verdict %s (%v), want %s", got, reasons, tt.want)
			}
			if len(reasons) == 0 {
				t.Fatal("a verdict needs a reason")
			}
		})
	}
}

func TestJudgeWarnings(t *testing.T) {
	tests := []struct {
		name     string
		rep      Report
		scenario string
		want     string
	}{
		{"cached", Report{Admitted: 10, Provider: mock.Snapshot{Requests: 2}}, Burst, "answered without it"},
		{"retries", Report{Admitted: 2, Provider: mock.Snapshot{Requests: 6}}, Burst, "already spent"},
		{"no overlap", Report{Sent: 5, Provider: mock.Snapshot{MaxInFlight: 1}}, Burst, "never overlapped"},
		{"no disconnects", Report{Admitted: 1, Provider: mock.Snapshot{Requests: 1}}, Disconnect, "no disconnect reached"},
		{"wrong route", Report{Provider: mock.Snapshot{Unrouted: 2, LastUnrouted: "POST /v1/responses"}}, Sequential, "/v1/responses"},
		{"bad calls", Report{Provider: mock.Snapshot{Rejected: 2}}, Sequential, "malformed"},
		{"broken", Report{BrokenStreams: 2}, Sequential, "failed partway"},
		{"false block", Report{Admitted: 1, Billed: 10, UsablePct: 1, Statuses: map[string]int{"200": 1, "429": 9}, Provider: mock.Snapshot{Requests: 1, Billed: 10}}, Sequential, "usable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.rep.Cap = 1000
			_, _, warnings := judge(&tt.rep, tt.scenario)
			if !strings.Contains(strings.Join(warnings, "\n"), tt.want) {
				t.Fatalf("warnings %v do not mention %q", warnings, tt.want)
			}
		})
	}
}

func TestSetupErrorsAreNotVerdicts(t *testing.T) {
	ts := startMock(t)
	good := func() Options {
		return Options{Scenario: Burst, Target: ts.URL, Oracle: ts.URL, Cap: 4_500_000_000, Requests: 30}
	}
	tests := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"unknown scenario", func(o *Options) { o.Scenario = "storm" }, "unknown scenario"},
		{"unknown format", func(o *Options) { o.Format = "cohere" }, "unknown format"},
		{"bad target", func(o *Options) { o.Target = "ftp://x" }, "target"},
		{"bad oracle", func(o *Options) { o.Oracle = "" }, "oracle"},
		{"zero cap", func(o *Options) { o.Cap = 0 }, "above zero"},
		{"too much concurrency", func(o *Options) { o.Concurrency = 31 }, "concurrency"},
		{"parallel sequential", func(o *Options) { o.Scenario = Sequential; o.Concurrency = 2 }, "concurrency 1"},
		{"bad header", func(o *Options) { o.Headers = []string{"nocolon"} }, "Name: value"},
		{"host header", func(o *Options) { o.Headers = []string{"Host: x"} }, "Host"},
		{"tiny prompt", func(o *Options) { o.PromptBytes = 10 }, "prompt"},
		{"cap below one call", func(o *Options) { o.Cap = 1 }, "more than the"},
		{"test cannot fail", func(o *Options) { o.Cap = 9_000_000_000_000 }, "cannot fail"},
		{"oracle down", func(o *Options) { o.Oracle = "http://127.0.0.1:1" }, "oracle unreachable"},
		{"bad mock override", func(o *Options) { o.Mock = json.RawMessage(`{"usage":"x"}`) }, "refused reset"},
		{"free calls", func(o *Options) {
			o.Mock = json.RawMessage(`{"input_rate_pico_per_token":0,"output_rate_pico_per_token":0}`)
		}, "cost nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := good()
			tt.mutate(&o)
			_, err := Run(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestNoGatewayIsABreach(t *testing.T) {
	ts := startMock(t)
	for _, format := range []string{"openai", "anthropic"} {
		for _, stream := range []bool{false, true} {
			rep, err := Run(context.Background(), Options{
				Scenario: Burst, Target: ts.URL, Oracle: ts.URL, Format: format, Stream: stream,
				Cap: 4_500_000_000, Requests: 30,
			})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Verdict != Breached || rep.Provider.Requests != 30 || rep.Admitted != 30 {
				t.Fatalf("%s stream=%t:\n%s", format, stream, rep.Text())
			}
			if rep.Overshoot != rep.Billed-rep.Cap || rep.OvershootPct <= 0 {
				t.Fatalf("overshoot not computed:\n%s", rep.Text())
			}
			if rep.Provider.MaxInFlight < 2 {
				t.Fatalf("the burst never overlapped:\n%s", rep.Text())
			}
		}
	}
}

func TestDeadTargetIsInconclusive(t *testing.T) {
	ts := startMock(t)
	rep, err := Run(context.Background(), Options{
		Scenario: Burst, Target: "http://127.0.0.1:1", Oracle: ts.URL, Cap: 4_500_000_000, Requests: 30,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Inconclusive || rep.TransportErrors != 30 {
		t.Fatalf("\n%s", rep.Text())
	}
}

func TestGatewayStatusOutcomes(t *testing.T) {
	ts := startMock(t)
	for _, tt := range []struct {
		status int
		want   string
	}{{401, Inconclusive}, {404, Inconclusive}, {500, Inconclusive}, {429, Inconclusive}, {302, Inconclusive}} {
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://example.invalid/")
			w.WriteHeader(tt.status)
			_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
		}))
		rep, err := Run(context.Background(), Options{Scenario: Sequential, Target: gw.URL, Oracle: ts.URL, Cap: 4_500_000_000, Requests: 15})
		gw.Close()
		if err != nil {
			t.Fatal(err)
		}
		if rep.Verdict != tt.want {
			t.Fatalf("status %d:\n%s", tt.status, rep.Text())
		}
		if sample := rep.Samples[strconv.Itoa(tt.status)]; sample == "" || len(sample) > 400 {
			t.Fatalf("sample should be present and truncated, got %d bytes", len(sample))
		}
	}
}

func TestPromptsAreUniqueAndEqualLength(t *testing.T) {
	r := &run{o: Options{RunID: "capproof-abc", PromptBytes: 200}}
	a, b := r.messages(1), r.messages(2)
	if string(a) == string(b) {
		t.Fatal("identical prompts would let a cache answer")
	}
	if len(a) != len(b) {
		t.Fatalf("prompt lengths differ: %d and %d", len(a), len(b))
	}
	var parsed []map[string]string
	if err := json.Unmarshal(a, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := len(parsed[0]["content"]); got != 200 {
		t.Fatalf("prompt is %d bytes, want 200", got)
	}
}

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"http://gw:1":     "http://gw:1/v1/chat/completions",
		"http://gw:1/":    "http://gw:1/v1/chat/completions",
		"http://gw:1/v1":  "http://gw:1/v1/chat/completions",
		"http://gw:1/v1/": "http://gw:1/v1/chat/completions",
		"http://gw:1/x":   "http://gw:1/x/v1/chat/completions",
	} {
		if got := endpoint(in, "openai"); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", in, got, want)
		}
	}
	if got := endpoint("http://gw:1/v1", "anthropic"); got != "http://gw:1/v1/messages" {
		t.Errorf("anthropic endpoint = %q", got)
	}
}

func TestHeadersExpandRunIDAndAuthByFormat(t *testing.T) {
	h := buildHeaders(Options{Format: "openai", APIKey: "k", RunID: "r1", Headers: []string{"X-Run-Id: {run}", "X-Note: a:b"}})
	if h.Get("Authorization") != "Bearer k" || h.Get("X-Run-Id") != "r1" || h.Get("X-Note") != "a:b" {
		t.Fatalf("%v", h)
	}
	h = buildHeaders(Options{Format: "anthropic", APIKey: "k"})
	if h.Get("x-api-key") != "k" || h.Get("Authorization") != "" || h.Get("anthropic-version") == "" {
		t.Fatalf("%v", h)
	}
}
