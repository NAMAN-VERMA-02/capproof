package lab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
)

const fakeKey = "sk-lab-fake"

func mockProvider(t *testing.T, change func(*mock.Config)) (*mock.Server, string) {
	t.Helper()
	cfg := mock.DefaultConfig()
	cfg.LatencyMS, cfg.ChunkDelayMS, cfg.OutputTokens = 0, 2, 100
	if change != nil {
		change(&cfg)
	}
	s, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func options(url string) Options {
	return Options{
		Provider: "openai", BaseURL: url, Model: "m", APIKey: fakeKey,
		InputRate: 150_000, OutputRate: 600_000, Phase: Full, Calls: 3, MaxTokens: 50, Confirm: true,
	}
}

func waitIdle(t *testing.T, s *mock.Server) mock.Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.Snapshot().InFlight > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the provider never went idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.Snapshot()
}

func TestNothingIsSentWithoutConfirm(t *testing.T) {
	s, url := mockProvider(t, nil)
	o := options(url)
	o.Confirm = false
	o.APIKey = "" // a dry run must not need a key
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Sent != 0 || s.Snapshot().Requests != 0 {
		t.Fatalf("a dry run reached the provider: %+v", rep)
	}
	if !strings.Contains(rep.Text(), "DRY RUN") || rep.WorstCase <= 0 {
		t.Fatalf("\n%s", rep.Text())
	}
}

func TestSpendChecksStopTheRunBeforeAnyCall(t *testing.T) {
	s, url := mockProvider(t, nil)
	for name, mutate := range map[string]func(*Options){
		"over the limit":      func(o *Options) { o.MaxSpend = 1 },
		"over the ceiling":    func(o *Options) { o.MaxSpend = HardCeiling + 1 },
		"expensive model":     func(o *Options) { o.OutputRate = 1 << 50 },
		"no prices":           func(o *Options) { o.InputRate, o.OutputRate = 0, 0 },
		"no key":              func(o *Options) { o.APIKey = "" },
		"no model":            func(o *Options) { o.Model = "" },
		"too many calls":      func(o *Options) { o.Calls = 1001 },
		"too many tokens":     func(o *Options) { o.MaxTokens = 4097 },
		"unknown phase":       func(o *Options) { o.Phase = "storm" },
		"unsupported":         func(o *Options) { o.Provider = "gemini" },
		"key to another host": func(o *Options) { o.BaseURL = "https://evil.example" },
		"key over http":       func(o *Options) { o.BaseURL = "http://api.openai.com" },
		"lookalike host":      func(o *Options) { o.BaseURL = "https://api.openai.com.evil.example" },
		"credentials in URL":  func(o *Options) { o.BaseURL = "https://u:p@api.openai.com" },
		"wrong provider host": func(o *Options) { o.BaseURL = "https://api.anthropic.com" },
	} {
		o := options(url)
		mutate(&o)
		if _, err := Run(context.Background(), o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if s.Snapshot().Requests != 0 {
		t.Fatal("a refused run reached the provider")
	}
}

func TestOfficialHostsAreAllowed(t *testing.T) {
	for provider, url := range map[string]string{"openai": "https://api.openai.com", "anthropic": "https://api.anthropic.com/"} {
		if err := checkHost(url, provider); err != nil {
			t.Errorf("%s: %v", provider, err)
		}
	}
}

func TestDefaultsAreCheap(t *testing.T) {
	o := Options{Provider: "openai", InputRate: 150_000, OutputRate: 600_000, Model: "gpt-4o-mini", Phase: Full}
	o.setDefaults()
	if o.BaseURL != "https://api.openai.com" {
		t.Fatalf("base URL %q", o.BaseURL)
	}
	if err := o.validate(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
	if got := o.WorstCase(); got > 10_000_000_000 { // one cent at these prices
		t.Fatalf("the default run risks %s", got)
	}
}

func TestFullPhaseMatchesTheBill(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			s, url := mockProvider(t, nil)
			o := options(url)
			o.Provider = provider
			rep, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			got := waitIdle(t, s)
			if rep.Sent != 3 || rep.Finished != 3 || rep.UnknownCost != 0 {
				t.Fatalf("%+v", rep)
			}
			if rep.Reported != got.Billed {
				t.Fatalf("reported %s, billed %s", rep.Reported, got.Billed)
			}
			if rep.Reported > rep.WorstCase {
				t.Fatalf("reported %s above the worst case %s", rep.Reported, rep.WorstCase)
			}
		})
	}
}

// This is the question the lab exists to answer: both behaviours must be visible.
func TestCancelPhaseSeparatesBillingBehaviours(t *testing.T) {
	bill := func(mode string) int64 {
		s, url := mockProvider(t, func(c *mock.Config) {
			c.OnDisconnect = mode
			c.Chunks = 20
			c.ChunkDelayMS = 10
		})
		rep, err := Run(context.Background(), func() Options { o := options(url); o.Phase = Cancel; return o }())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Cancelled != 3 || rep.Finished != 0 {
			t.Fatalf("%+v", rep)
		}
		got := waitIdle(t, s)
		if got.Disconnects != 3 {
			t.Fatalf("%d disconnects reached the provider", got.Disconnects)
		}
		return got.OutputTokens
	}
	full, delivered := bill("full"), bill("delivered")
	if full != 150 {
		t.Fatalf("full mode billed %d output tokens, want 150", full)
	}
	if delivered >= full/2 {
		t.Fatalf("delivered mode billed %d tokens against %d: the lab could not tell them apart", delivered, full)
	}
}

func TestCapPhaseStopsAtTheFirstRefusalWithoutRetrying(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) > 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("limit reached for key " + fakeKey + "\x1b[2J\nverdict HELD"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer s.Close()
	o := options(s.URL)
	o.Phase, o.Calls = Cap, 50
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || rep.Finished != 2 || rep.RefusedStatus != 429 {
		t.Fatalf("%d calls, %+v", calls.Load(), rep)
	}
	raw, _ := json.Marshal(rep)
	for _, output := range []string{rep.Text(), string(raw)} {
		if strings.Contains(output, fakeKey) {
			t.Errorf("the key leaked:\n%s", output)
		}
	}
	if strings.Contains(rep.Text(), "\x1b") || strings.Contains(rep.Text(), "\nverdict HELD") {
		t.Errorf("provider text was not neutralised:\n%q", rep.Text())
	}
}

func TestStreamCutOffCountsAsUnknown(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"))
	}))
	defer s.Close()
	rep, err := Run(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Finished != 0 || rep.UnknownCost != 3 || rep.Reported != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestAuthFailureStopsAtOnce(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer s.Close()
	rep, err := Run(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || rep.RefusedStatus != 401 {
		t.Fatalf("%d calls, %+v", calls.Load(), rep)
	}
}

func TestRedirectsDoNotCarryTheKey(t *testing.T) {
	var leaked atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), fakeKey) {
			leaked.Add(1)
		}
	}))
	defer elsewhere.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer s.Close()
	rep, err := Run(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	if leaked.Load() != 0 || rep.RefusedStatus != http.StatusTemporaryRedirect {
		t.Fatalf("leaked %d, %+v", leaked.Load(), rep)
	}
}

func TestDeadProviderStopsAtOnce(t *testing.T) {
	o := options("http://127.0.0.1:1")
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sent != 1 || rep.FailedError == "" {
		t.Fatalf("%+v", rep)
	}
}
