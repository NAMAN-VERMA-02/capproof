package capd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
	"github.com/NAMAN-VERMA-02/capproof/internal/runner"
)

const (
	testKey = "sk-capd-test"
	testCap = money.Pico(4_500_000_000)
)

type rig struct {
	server   *Server
	ledger   *Ledger
	provider *mock.Server
	target   string
	oracle   string
}

func newRig(t *testing.T, k KeyConfig) rig {
	t.Helper()
	cfg := mock.DefaultConfig()
	cfg.LatencyMS, cfg.ChunkDelayMS = 20, 1
	provider, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(provider.Handler())
	t.Cleanup(upstream.Close)
	ledger, _, err := Open(filepath.Join(t.TempDir(), "ledger test.db"), "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	if k.Name == "" {
		k.Name = "ci"
	}
	if k.BudgetUSD == "" {
		k.BudgetUSD = "0.0045"
	}
	k.SHA256 = HashKey(testKey)
	server, err := New(Config{
		OpenAIURL: upstream.URL, AnthropicURL: upstream.URL, Keys: []KeyConfig{k},
		Prices: map[string]ModelPrice{"gpt-4o-mini": {Input: "0.15", Output: "0.60", CacheWrite: "0.15"}},
	}, ledger)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(server)
	t.Cleanup(front.Close)
	return rig{server: server, ledger: ledger, provider: provider, target: front.URL, oracle: upstream.URL}
}

func (r rig) run(t *testing.T, o runner.Options) *runner.Report {
	t.Helper()
	o.Target, o.Oracle, o.Cap, o.APIKey = r.target, r.oracle, testCap, testKey
	o.Timeout = 20 * time.Second
	rep, err := runner.Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (r rig) checkInvariants(t *testing.T, id string, rep *runner.Report) {
	t.Helper()
	b, err := r.ledger.Balance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if b.Held != 0 {
		t.Errorf("I4: %s still held", b.Held)
	}
	if b.Spent+b.Held > testCap {
		t.Errorf("I1: spent %s + held %s past the cap", b.Spent, b.Held)
	}
	if rep.Billed > testCap {
		t.Errorf("I2: billed %s past the cap", rep.Billed)
	}
	if b.Spent < rep.Billed {
		t.Errorf("I3: ledger %s below the bill %s", b.Spent, rep.Billed)
	}
}

func TestCapHoldsUnderEveryFault(t *testing.T) {
	tests := []struct {
		name string
		o    runner.Options
	}{
		{"burst", runner.Options{Scenario: runner.Burst, Requests: 30, Wave2: 5, Settle: time.Millisecond}},
		{"burst stream", runner.Options{Scenario: runner.Burst, Requests: 30, Wave2: 5, Settle: time.Millisecond, Stream: true}},
		{"burst anthropic", runner.Options{Scenario: runner.Burst, Requests: 30, Wave2: 5, Settle: time.Millisecond, Format: "anthropic"}},
		{"burst anthropic stream", runner.Options{Scenario: runner.Burst, Requests: 30, Wave2: 5, Settle: time.Millisecond, Format: "anthropic", Stream: true}},
		{"sequential", runner.Options{Scenario: runner.Sequential, Requests: 15}},
		{"disconnect", runner.Options{Scenario: runner.Disconnect, Requests: 30}},
		{"disconnect anthropic", runner.Options{Scenario: runner.Disconnect, Requests: 30, Format: "anthropic"}},
		{"usage omitted", runner.Options{Scenario: runner.Sequential, Requests: 20, Mock: json.RawMessage(`{"usage":"omit"}`)}},
		{"usage malformed", runner.Options{Scenario: runner.Sequential, Requests: 20, Mock: json.RawMessage(`{"usage":"malformed"}`)}},
		{"stream usage omitted", runner.Options{Scenario: runner.Sequential, Requests: 20, Stream: true, Mock: json.RawMessage(`{"usage":"omit"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, KeyConfig{})
			rep := r.run(t, tt.o)
			if rep.Verdict != runner.Held {
				t.Fatalf("\n%s", rep.Text())
			}
			r.checkInvariants(t, "key:ci", rep)
		})
	}
}

func TestRunBudgetIsSeparateFromTheKeyBudget(t *testing.T) {
	r := newRig(t, KeyConfig{BudgetUSD: "1", RunBudgetUSD: "0.0045"})
	rep := r.run(t, runner.Options{Scenario: runner.Burst, Requests: 30, Wave2: 5, Settle: time.Millisecond, Headers: []string{"X-Run-Id: {run}"}})
	if rep.Verdict != runner.Held {
		t.Fatalf("\n%s", rep.Text())
	}
	r.checkInvariants(t, "run:ci:"+rep.RunID, rep)
}

func TestKeyBudgetHoldsAcrossManyRuns(t *testing.T) {
	r := newRig(t, KeyConfig{RunBudgetUSD: "1"})
	client := http.Client{}
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := chatRequest(r.target+"/r/run-"+string(rune('a'+i))+"/v1/chat/completions", `{"model":"gpt-4o-mini","max_tokens":500,"messages":[{"role":"user","content":"hi"}]}`)
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
	}
	wg.Wait()
	if billed := r.provider.Snapshot().Billed; billed > testCap {
		t.Fatalf("fresh run IDs escaped the key budget: billed %s", billed)
	}
}

func chatRequest(url, body string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	return req
}

func do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

const goodCall = `{"model":"gpt-4o-mini","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`

func TestStepLimit(t *testing.T) {
	r := newRig(t, KeyConfig{MaxRunSteps: 2})
	url := r.target + "/r/job-1/v1/chat/completions"
	for i, want := range []int{200, 200, 429} {
		if status, body := do(t, chatRequest(url, goodCall)); status != want {
			t.Fatalf("call %d: status %d: %s", i, status, body)
		}
	}
	if status, _ := do(t, chatRequest(r.target+"/r/job-2/v1/chat/completions", goodCall)); status != 200 {
		t.Fatalf("another run was blocked: %d", status)
	}
}

func TestVelocityLimit(t *testing.T) {
	r := newRig(t, KeyConfig{BudgetUSD: "1", USDPerMinute: "0.0001"})
	url := r.target + "/v1/chat/completions"
	if status, body := do(t, chatRequest(url, goodCall)); status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	saw429 := false
	for range 50 {
		status, body := do(t, chatRequest(url, goodCall))
		if status == 429 && strings.Contains(body, "velocity_limit") {
			saw429 = true
			break
		}
	}
	if !saw429 {
		t.Fatal("the velocity limit never fired")
	}
}

func TestRefusalsWithoutForwarding(t *testing.T) {
	tests := []struct {
		name, path, body string
		status           int
	}{
		{"unknown model", "/v1/chat/completions", `{"model":"gpt-free","max_tokens":5,"messages":[{}]}`, 400},
		{"no limit", "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{}]}`, 400},
		{"duplicate model", "/v1/chat/completions", `{"model":"gpt-free","model":"gpt-4o-mini","max_tokens":5}`, 400},
		{"smuggled limit", "/v1/chat/completions", `{"model":"gpt-4o-mini","max_tokens":5000000,"MAX_TOKENS":1}`, 400},
		{"priority tier", "/v1/chat/completions", `{"model":"gpt-4o-mini","max_tokens":5,"service_tier":"priority"}`, 400},
		{"not json", "/v1/messages", `nope`, 400},
		{"unknown route", "/v1/responses", goodCall, 404},
		{"oracle route", "/_oracle/reset", ``, 404},
		{"traversal", "/v1/chat/completions/../../_oracle", goodCall, 404},
		{"bad run id", "/r/bad$id/v1/chat/completions", goodCall, 400},
		{"run id with no path", "/r/onlyrun", goodCall, 400},
		{"huge request limit is refused by the budget", "/v1/chat/completions", `{"model":"gpt-4o-mini","max_tokens":9000000000000000000,"messages":[{}]}`, 402},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, KeyConfig{})
			if status, body := do(t, chatRequest(r.target+tt.path, tt.body)); status != tt.status {
				t.Fatalf("status %d, want %d: %s", status, tt.status, body)
			}
			if got := r.provider.Snapshot(); got.Requests != 0 || got.Rejected != 0 || got.Unrouted != 0 {
				t.Fatalf("the call reached the provider: %+v", got)
			}
		})
	}
}

func TestAuthentication(t *testing.T) {
	r := newRig(t, KeyConfig{})
	for name, set := range map[string]func(*http.Request){
		"none":        func(req *http.Request) { req.Header.Del("Authorization") },
		"wrong":       func(req *http.Request) { req.Header.Set("Authorization", "Bearer nope") },
		"wrong x-api": func(req *http.Request) { req.Header.Del("Authorization"); req.Header.Set("x-api-key", "nope") },
		"not bearer":  func(req *http.Request) { req.Header.Set("Authorization", testKey) },
	} {
		req := chatRequest(r.target+"/v1/chat/completions", goodCall)
		set(req)
		if status, _ := do(t, req); status != 401 {
			t.Errorf("%s: status %d", name, status)
		}
	}
	if r.provider.Snapshot().Requests != 0 {
		t.Fatal("an unauthenticated call reached the provider")
	}
	req := chatRequest(r.target+"/v1/messages", goodCall)
	req.Header.Del("Authorization")
	req.Header.Set("x-api-key", testKey)
	if status, body := do(t, req); status != 200 {
		t.Fatalf("x-api-key refused: %d %s", status, body)
	}
}

func TestVirtualKeyNeverReachesTheProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-real-provider")
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	ledger, _, err := Open(filepath.Join(t.TempDir(), "l.db"), "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	server, err := New(Config{
		OpenAIURL: upstream.URL,
		Keys:      []KeyConfig{{Name: "ci", SHA256: HashKey(testKey), BudgetUSD: "1"}},
		Prices:    map[string]ModelPrice{"gpt-4o-mini": {Input: "1", Output: "1"}},
	}, ledger)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(server)
	defer front.Close()
	req := chatRequest(front.URL+"/v1/chat/completions", goodCall)
	req.Header.Set("Cookie", "secret=1")
	req.Header.Set("X-Run-Id", "r1")
	req.Header.Set("anthropic-beta", "keep-me")
	do(t, req)
	if got.Get("Authorization") != "Bearer sk-real-provider" {
		t.Errorf("Authorization upstream = %q", got.Get("Authorization"))
	}
	for name, values := range got {
		for _, value := range values {
			if strings.Contains(value, testKey) {
				t.Errorf("the virtual key leaked in %s", name)
			}
		}
	}
	if got.Get("Cookie") != "" || got.Get("X-Run-Id") != "" {
		t.Errorf("private headers crossed: %v", got)
	}
	if got.Get("Anthropic-Beta") != "keep-me" {
		t.Error("anthropic-beta must pass through verbatim")
	}
}

func TestStreamsAskForUsage(t *testing.T) {
	r := newRig(t, KeyConfig{BudgetUSD: "1"})
	status, body := do(t, chatRequest(r.target+"/v1/chat/completions",
		`{"model":"gpt-4o-mini","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if status != 200 || !strings.Contains(body, `"completion_tokens":5`) {
		t.Fatalf("status %d:\n%s", status, body)
	}
	b, err := r.ledger.Balance(context.Background(), "key:ci")
	if err != nil {
		t.Fatal(err)
	}
	if billed := r.provider.Snapshot().Billed; b.Spent != billed {
		t.Fatalf("ledger %s, provider %s: the true cost should settle", b.Spent, billed)
	}
}

func TestLedgerFailsClosed(t *testing.T) {
	r := newRig(t, KeyConfig{})
	r.ledger.db.Close() // the disk failing or the file vanishing looks like this
	status, body := do(t, chatRequest(r.target+"/v1/chat/completions", goodCall))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", status, body)
	}
	if r.provider.Snapshot().Requests != 0 {
		t.Fatal("the call was forwarded without a durable hold")
	}
}

func TestBadConfigsAreRefused(t *testing.T) {
	ledger, _, err := Open(filepath.Join(t.TempDir(), "l.db"), "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	good := func() Config {
		return Config{
			OpenAIURL: "http://x", Keys: []KeyConfig{{Name: "a", SHA256: HashKey("a"), BudgetUSD: "1"}},
			Prices: map[string]ModelPrice{"m": {Input: "1", Output: "1"}},
		}
	}
	if _, err := New(good(), ledger); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"no upstream":   func(c *Config) { c.OpenAIURL = "" },
		"file upstream": func(c *Config) { c.OpenAIURL = "file:///etc/passwd" },
		"no keys":       func(c *Config) { c.Keys = nil },
		"no prices":     func(c *Config) { c.Prices = nil },
		"zero budget":   func(c *Config) { c.Keys[0].BudgetUSD = "0" },
		"no name":       func(c *Config) { c.Keys[0].Name = "" },
		"raw key":       func(c *Config) { c.Keys[0].SHA256 = "sk-plain" },
		"duplicate key": func(c *Config) { c.Keys = append(c.Keys, c.Keys[0]) },
		"free model":    func(c *Config) { c.Prices["m"] = ModelPrice{Input: "0", Output: "0"} },
		"bad price":     func(c *Config) { c.Prices["m"] = ModelPrice{Input: "x", Output: "1"} },
		"bad bytes":     func(c *Config) { c.BytesPerToken = -1 },
	} {
		cfg := good()
		mutate(&cfg)
		if _, err := New(cfg, ledger); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSynchronousSettingIsChecked(t *testing.T) {
	if _, _, err := Open(filepath.Join(t.TempDir(), "l.db"), "OFF"); err == nil {
		t.Fatal("synchronous=OFF accepted")
	}
}

func TestLedgerRules(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "l.db")
	l, _, err := Open(path, "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	scope := []Scope{{ID: "key:a", Cap: 100}}

	id, d, err := l.Reserve(ctx, scope, 60)
	if err != nil || d != Admitted {
		t.Fatalf("%v %v", d, err)
	}
	if _, d, _ := l.Reserve(ctx, scope, 60); d != HeldBack {
		t.Fatalf("decision %v, want HeldBack", d)
	}
	if err := l.Settle(ctx, id, 50); err != nil {
		t.Fatal(err)
	}
	if err := l.Settle(ctx, id, 50); err != ErrSettled {
		t.Fatalf("double settle: %v", err)
	}
	if err := l.Capture(ctx, id); err != ErrSettled {
		t.Fatalf("capture after settle: %v", err)
	}
	b, _ := l.Balance(ctx, "key:a")
	if b.Spent != 50 || b.Held != 0 || b.Steps != 1 {
		t.Fatalf("%+v", b)
	}
	if _, d, _ := l.Reserve(ctx, scope, 60); d != Exhausted {
		t.Fatalf("decision %v, want Exhausted", d)
	}

	// All or nothing: a refusal by the second scope must not touch the first.
	if _, d, _ := l.Reserve(ctx, []Scope{{ID: "key:a", Cap: 100}, {ID: "run:x", Cap: 1}}, 10); d != Exhausted {
		t.Fatalf("decision %v", d)
	}
	if b, _ := l.Balance(ctx, "key:a"); b.Held != 0 || b.Steps != 1 {
		t.Fatalf("a refused call changed the other scope: %+v", b)
	}

	// A released hold charges nothing.
	id, _, _ = l.Reserve(ctx, scope, 10)
	if err := l.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	if b, _ := l.Balance(ctx, "key:a"); b.Spent != 50 || b.Held != 0 {
		t.Fatalf("%+v", b)
	}

	// An open hold survives a restart and is charged in full.
	if _, _, err := l.Reserve(ctx, scope, 7); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, recovered, err := Open(path, "FULL")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if recovered != 1 {
		t.Fatalf("recovered %d", recovered)
	}
	if b, _ := l.Balance(ctx, "key:a"); b.Spent != 57 || b.Held != 0 {
		t.Fatalf("%+v", b)
	}
}

func TestConcurrentReservationsNeverOverbook(t *testing.T) {
	ctx := context.Background()
	l, _, err := Open(filepath.Join(t.TempDir(), "l.db"), "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	scope := []Scope{{ID: "key:a", Cap: 1000}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, d, err := l.Reserve(ctx, scope, 10)
			if err != nil {
				t.Error(err)
				return
			}
			if d == Admitted {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != 100 {
		t.Fatalf("admitted %d, want exactly 100", admitted)
	}
}

func TestSplitRun(t *testing.T) {
	for _, tt := range []struct {
		in, rest, run string
		ok            bool
	}{
		{"/v1/messages", "/v1/messages", "", true},
		{"/r/job-1/v1/messages", "/v1/messages", "job-1", true},
		{"/r/a.b:c_d/v1/x", "/v1/x", "a.b:c_d", true},
		{"/r/", "", "", false},
		{"/r/job", "", "", false},
		{"/r/" + strings.Repeat("a", 129) + "/v1/x", "", "", false},
		{"/r/a b/v1/x", "", "", false},
	} {
		rest, run, ok := splitRun(tt.in)
		if ok != tt.ok || (ok && (rest != tt.rest || run != tt.run)) {
			t.Errorf("splitRun(%q) = %q, %q, %t", tt.in, rest, run, ok)
		}
	}
}
