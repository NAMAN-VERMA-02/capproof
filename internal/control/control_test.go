package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
	"github.com/NAMAN-VERMA-02/capproof/internal/runner"
)

// capFor10Calls is worth just under ten calls at the default call cost, using the
// runner's defaults and the mock's gpt-4o-mini pricing.
const capFor10Calls = money.Pico(4_500_000_000)

type rig struct {
	gateway *Gateway
	target  string
	oracle  string
}

func newRig(t *testing.T, mode string) rig {
	t.Helper()
	cfg := mock.DefaultConfig()
	cfg.LatencyMS = 20
	cfg.ChunkDelayMS = 1
	provider, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(provider.Handler())
	t.Cleanup(upstream.Close)
	gw, err := New(mode, upstream.URL, capFor10Calls, cfg.InputRate, cfg.OutputRate)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(gw)
	t.Cleanup(front.Close)
	return rig{gateway: gw, target: front.URL, oracle: upstream.URL}
}

func (r rig) run(t *testing.T, o runner.Options) *runner.Report {
	t.Helper()
	o.Target, o.Oracle, o.Cap = r.target, r.oracle, capFor10Calls
	o.Timeout = 20 * time.Second
	rep, err := runner.Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// checkLedger enforces the invariants on the strict gateway's own books.
func (r rig) checkLedger(t *testing.T, rep *runner.Report) {
	t.Helper()
	spent, held := r.gateway.Ledger()
	if held != 0 {
		t.Errorf("I4: %s still held after the run", held)
	}
	if spent > capFor10Calls {
		t.Errorf("I1: ledger spent %s past the %s cap", spent, capFor10Calls)
	}
	if spent < rep.Billed {
		t.Errorf("I3: ledger recorded %s but the provider billed %s", spent, rep.Billed)
	}
}

// TestSuiteTellsControlsApart is the calibration: a scenario that fails to catch
// the naive gateway, or that fails the strict one, is not measuring anything.
func TestSuiteTellsControlsApart(t *testing.T) {
	tests := []struct {
		name      string
		o         runner.Options
		naiveWant string
	}{
		{"burst", runner.Options{Scenario: runner.Burst, Requests: 30}, runner.Breached},
		{"burst stream", runner.Options{Scenario: runner.Burst, Requests: 30, Stream: true}, runner.Breached},
		{"burst anthropic", runner.Options{Scenario: runner.Burst, Requests: 30, Format: "anthropic"}, runner.Breached},
		{"burst anthropic stream", runner.Options{Scenario: runner.Burst, Requests: 30, Format: "anthropic", Stream: true}, runner.Breached},
		{"disconnect", runner.Options{Scenario: runner.Disconnect, Requests: 30}, runner.Breached},
		{"disconnect anthropic", runner.Options{Scenario: runner.Disconnect, Requests: 30, Format: "anthropic"}, runner.Breached},
		{"usage omitted", runner.Options{Scenario: runner.Sequential, Requests: 20, Mock: json.RawMessage(`{"usage":"omit"}`)}, runner.Breached},
		{"usage malformed", runner.Options{Scenario: runner.Sequential, Requests: 20, Mock: json.RawMessage(`{"usage":"malformed"}`)}, runner.Breached},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := newRig(t, Naive).run(t, tt.o)
			if rep.Verdict != tt.naiveWant {
				t.Errorf("naive gateway:\n%s", rep.Text())
			}

			strict := newRig(t, Strict)
			rep = strict.run(t, tt.o)
			if rep.Verdict != runner.Held {
				t.Errorf("strict gateway:\n%s", rep.Text())
			}
			strict.checkLedger(t, rep)
		})
	}
}

// Calls run one at a time leave no race for the naive design, so it may overshoot
// by at most the call that crossed the line. This pins down what burst adds.
func TestNaiveSequentialOvershootsByOneCall(t *testing.T) {
	rep := newRig(t, Naive).run(t, runner.Options{Scenario: runner.Sequential, Requests: 15})
	if rep.Provider.Requests > rep.CallsCapAllows+1 {
		t.Fatalf("\n%s", rep.Text())
	}
}

func TestStrictUsesMostOfTheBudget(t *testing.T) {
	strict := newRig(t, Strict)
	rep := strict.run(t, runner.Options{Scenario: runner.Sequential, Requests: 15})
	if rep.Verdict != runner.Held || rep.UsablePct < 85 {
		t.Fatalf("\n%s", rep.Text())
	}
	strict.checkLedger(t, rep)
}

func TestStrictReleasesBudgetAfterBurst(t *testing.T) {
	strict := newRig(t, Strict)
	rep := strict.run(t, runner.Options{Scenario: runner.Burst, Requests: 30, Wave2: 5, Settle: time.Millisecond})
	if rep.Verdict != runner.Held {
		t.Fatalf("\n%s", rep.Text())
	}
	strict.checkLedger(t, rep)
}

// A provider that ignores max_tokens breaks the assumption every hold relies on.
// The suite must report that as a breach rather than hide it.
func TestProviderIgnoringLimitBreaksEvenStrict(t *testing.T) {
	rep := newRig(t, Strict).run(t, runner.Options{
		Scenario: runner.Burst, Requests: 30, MaxTokens: 1,
		Mock: json.RawMessage(`{"ignore_max_tokens":true}`),
	})
	if rep.Verdict != runner.Breached {
		t.Fatalf("\n%s", rep.Text())
	}
}

func TestStrictRefusesCallsWithoutALimit(t *testing.T) {
	r := newRig(t, Strict)
	resp, err := http.Post(r.target+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestProviderRefusalReleasesTheHold(t *testing.T) {
	r := newRig(t, Strict)
	// max_tokens is fine for the gateway, but the provider refuses the empty message list.
	resp, err := http.Post(r.target+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[],"max_tokens":5}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if spent, held := r.gateway.Ledger(); spent != 0 || held != 0 {
		t.Fatalf("spent %s, held %s after a refused call", spent, held)
	}
}

func TestUnknownRoutesAreNotForwarded(t *testing.T) {
	r := newRig(t, Strict)
	resp, err := http.Post(r.target+"/_oracle/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestNewRejectsBadSettings(t *testing.T) {
	for _, tt := range []struct {
		mode, upstream string
		limit          money.Pico
	}{
		{"loose", "http://x", 1},
		{Strict, "not a url", 1},
		{Strict, "file:///etc/passwd", 1},
		{Strict, "http://x", 0},
	} {
		if _, err := New(tt.mode, tt.upstream, tt.limit, 1, 1); err == nil {
			t.Errorf("New(%q, %q, %d) accepted", tt.mode, tt.upstream, tt.limit)
		}
	}
}

func TestUsageTracker(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  money.Pico
		known bool
	}{
		{"openai", []string{`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`}, 10*2 + 5*3, true},
		{"anthropic cumulative keeps the last value", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"message_delta","usage":{"output_tokens":3}}`,
			`{"type":"message_delta","usage":{"output_tokens":5}}`,
		}, 10*2 + 5*3, true},
		{"anthropic input can grow within a stream", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"message_delta","usage":{"input_tokens":40,"output_tokens":5}}`,
		}, 40*2 + 5*3, true},
		{"nothing", []string{`{"choices":[]}`, `[DONE]`, ``}, 0, false},
		{"null usage", []string{`{"usage":null}`}, 0, false},
		{"input only", []string{`{"usage":{"prompt_tokens":10}}`}, 0, false},
		{"wrong types", []string{`{"usage":{"prompt_tokens":"oops","completion_tokens":null}}`}, 0, false},
		{"negative", []string{`{"usage":{"prompt_tokens":-1,"completion_tokens":1}}`}, 0, false},
		{"good then malformed stays unknown", []string{
			`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			`{"usage":{"prompt_tokens":"oops"}}`,
		}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := usageTracker{inputRate: 2, outputRate: 3}
			for _, line := range tt.lines {
				u.feed([]byte(line))
			}
			got, known := u.cost()
			if got != tt.want || known != tt.known {
				t.Fatalf("cost = %d, %t; want %d, %t", got, known, tt.want, tt.known)
			}
		})
	}
}
