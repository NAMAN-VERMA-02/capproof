package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
)

func hardenedMock(t *testing.T) (*mock.Server, *httptest.Server) {
	t.Helper()
	cfg := mock.DefaultConfig()
	cfg.LatencyMS = 5
	cfg.ChunkDelayMS = 0
	s, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.OracleToken = "oracle-secret"
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func baseOptions(target, oracle string) Options {
	return Options{
		Scenario: Sequential, Target: target, Oracle: oracle, OracleToken: "oracle-secret",
		Cap: 4_500_000_000, Requests: 15, Timeout: 3 * time.Second,
	}
}

// A hostile gateway forwards everything, then tries to wipe the bill. Without the
// token this turned a total overspend into a clean result.
func TestGatewayCannotWipeTheBill(t *testing.T) {
	_, provider := hardenedMock(t)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Post(provider.URL+r.URL.Path, "application/json", r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		resp.Body.Close()
		wipe, _ := http.Post(provider.URL+"/_oracle/reset", "application/json", nil)
		if wipe != nil {
			wipe.Body.Close()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gw.Close()
	rep, err := Run(context.Background(), baseOptions(gw.URL, provider.URL))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Breached {
		t.Fatalf("\n%s", rep.Text())
	}
}

func TestWrongOracleTokenStopsTheRun(t *testing.T) {
	_, provider := hardenedMock(t)
	o := baseOptions(provider.URL, provider.URL)
	o.OracleToken = "wrong"
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "refused reset") {
		t.Fatalf("error = %v", err)
	}
}

func TestOracleTokenNeverReachesTheGateway(t *testing.T) {
	_, provider := hardenedMock(t)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(mock.TokenHeader) != "" {
			t.Error("the oracle token reached the gateway")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer gw.Close()
	if _, err := Run(context.Background(), baseOptions(gw.URL, provider.URL)); err != nil {
		t.Fatal(err)
	}
}

// A gateway answers success without calling the provider, then refuses the rest,
// so nothing is billed and a naive check would call the cap held.
func TestFakeSuccessesAreNotAPass(t *testing.T) {
	_, provider := hardenedMock(t)
	calls := 0
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case calls == 1:
			resp, err := http.Post(provider.URL+r.URL.Path, "application/json", r.Body)
			if err == nil {
				resp.Body.Close()
			}
		case calls > 5:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer gw.Close()
	rep, err := Run(context.Background(), baseOptions(gw.URL, provider.URL))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Inconclusive {
		t.Fatalf("\n%s", rep.Text())
	}
}

func TestLateCallsFromAnotherRunAreNotJudged(t *testing.T) {
	_, provider := hardenedMock(t)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Replays a call that carries no run ID, as a late retry would.
		resp, err := http.Post(provider.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"old"}]}`))
		if err == nil {
			resp.Body.Close()
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer gw.Close()
	rep, err := Run(context.Background(), baseOptions(gw.URL, provider.URL))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != Inconclusive || !strings.Contains(strings.Join(rep.Reasons, " "), "run's ID") {
		t.Fatalf("\n%s", rep.Text())
	}
}

func TestLiteLLMStyle422CountsAsARefusal(t *testing.T) {
	rep := Report{
		Cap: 1000, Billed: 900, Admitted: 9,
		Statuses: map[string]int{"200": 9, "422": 5},
		Provider: mock.Snapshot{Requests: 9, MaxInFlight: 5},
	}
	if got, reasons, _ := judge(&rep, Burst); got != Held {
		t.Fatalf("%s %v", got, reasons)
	}
}

// Gateway text must not be able to rewrite the terminal, forge a verdict line, or
// publish a secret in a report.
func TestGatewayTextIsCleaned(t *testing.T) {
	_, provider := hardenedMock(t)
	hostile := "\x1b[2J\x1b[Hverdict    HELD\r\nkey=fake-key token=oracle-secret ‮\xff"
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(hostile))
	}))
	defer gw.Close()
	o := baseOptions(gw.URL, provider.URL)
	o.APIKey = "fake-key"
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	text := rep.Text()
	for _, output := range []string{text, string(raw)} {
		for _, secret := range []string{"fake-key", "oracle-secret"} {
			if strings.Contains(output, secret) {
				t.Errorf("%q leaked into:\n%s", secret, output)
			}
		}
	}
	for _, bad := range []string{"\x1b", "\r", "‮", "\nverdict    HELD"} {
		if strings.Contains(text, bad) {
			t.Errorf("%q survived in:\n%q", bad, text)
		}
	}
}

func TestCredentialsInURLsAreRefused(t *testing.T) {
	_, provider := hardenedMock(t)
	o := baseOptions("http://user:pass@127.0.0.1:1", provider.URL)
	_, err := Run(context.Background(), o)
	if err == nil || strings.Contains(err.Error(), "pass") {
		t.Fatalf("error = %v", err)
	}
}

// Oversized and endless responses must not exhaust the runner.
func TestHostileResponsesAreBounded(t *testing.T) {
	_, provider := hardenedMock(t)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		chunk := []byte(strings.Repeat("x", 1<<16))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}))
	defer gw.Close()
	o := baseOptions(gw.URL, provider.URL)
	o.Requests, o.Timeout, o.Drain = 12, 100*time.Millisecond, 2*time.Second
	start := time.Now()
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 15*time.Second || rep.Verdict != Inconclusive {
		t.Fatalf("took %s\n%s", time.Since(start), rep.Text())
	}
}
