package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

func TestOutputLimit(t *testing.T) {
	tests := []struct {
		name, body string
		want       int64
		wantErr    bool
	}{
		{name: "plain", body: `{"max_tokens":5}`, want: 5},
		{name: "new name", body: `{"max_completion_tokens":7}`, want: 7},
		// Holding for the smaller of the two would undercount if the provider uses the larger.
		{name: "both take the larger", body: `{"max_tokens":1,"max_completion_tokens":9000}`, want: 9000},
		{name: "n of one", body: `{"max_tokens":5,"n":1}`, want: 5},
		{name: "nested does not count", body: `{"metadata":{"max_tokens":1},"max_tokens":5}`, want: 5},
		{name: "duplicate key", body: `{"max_tokens":9000,"max_tokens":1}`, wantErr: true},
		{name: "case variant", body: `{"max_tokens":9000,"MAX_TOKENS":1}`, wantErr: true},
		{name: "case variant alone", body: `{"Max_Tokens":1}`, wantErr: true},
		{name: "n above one", body: `{"max_tokens":5,"n":100}`, wantErr: true},
		{name: "fractional", body: `{"max_tokens":1.5}`, wantErr: true},
		{name: "string", body: `{"max_tokens":"5"}`, wantErr: true},
		{name: "null", body: `{"max_tokens":null}`, wantErr: true},
		{name: "zero", body: `{"max_tokens":0}`, wantErr: true},
		{name: "negative", body: `{"max_tokens":-1}`, wantErr: true},
		{name: "missing", body: `{"model":"m"}`, wantErr: true},
		{name: "array", body: `[{"max_tokens":5}]`, wantErr: true},
		{name: "truncated", body: `{"max_tokens":5`, wantErr: true},
		{name: "empty", body: ``, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := outputLimit([]byte(tt.body))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("outputLimit = %d, %v; want %d, error %t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// The attack: declare a one-token limit where the gateway looks, and a large one
// where the provider looks. The strict control must not be billed past its hold.
func TestLimitSmugglingCannotBeatTheHold(t *testing.T) {
	const messages = `"messages":[{"role":"user","content":"hi"}]`
	for _, body := range []string{
		`{"model":"m",` + messages + `,"max_tokens":1,"max_completion_tokens":500}`,
		`{"model":"m",` + messages + `,"max_completion_tokens":500,"max_tokens":1}`,
		`{"model":"m",` + messages + `,"max_tokens":500,"MAX_TOKENS":1}`,
		`{"model":"m",` + messages + `,"max_tokens":500,"max_tokens":1}`,
	} {
		cfg := mock.DefaultConfig()
		cfg.LatencyMS, cfg.ChunkDelayMS = 0, 0
		provider, err := mock.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		upstream := httptest.NewServer(provider.Handler())
		gw, err := New(Strict, upstream.URL, 1_000_000_000_000, cfg.InputRate, cfg.OutputRate)
		if err != nil {
			t.Fatal(err)
		}
		front := httptest.NewServer(gw)
		resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		spent, held := gw.Ledger()
		if billed := provider.Snapshot().Billed; billed > spent+held {
			t.Errorf("%s: provider billed %s but the gateway accounted %s", body, billed, spent+held)
		}
		front.Close()
		upstream.Close()
	}
}

// upstreamReturning is a fake provider with one fixed response.
func upstreamReturning(t *testing.T, status int, contentType, body string) *Gateway {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	gw, err := New(Strict, upstream.URL, 1_000_000_000_000, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

func callStrict(t *testing.T, gw *Gateway) (spent, hold money.Pico) {
	t.Helper()
	body := `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	spent, held := gw.Ledger()
	if held != 0 {
		t.Fatalf("%s still held", held)
	}
	return spent, money.Pico((len(body)+3)/4 + 1000)
}

func TestSettlementCannotUndercount(t *testing.T) {
	const sse = "text/event-stream"
	finished := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	cutOff := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"

	spent, _ := callStrict(t, upstreamReturning(t, 200, sse, finished))
	if spent != 5 {
		t.Errorf("finished stream charged %d, want the real 5", spent)
	}
	spent, hold := callStrict(t, upstreamReturning(t, 200, sse, cutOff))
	if spent != hold {
		t.Errorf("stream cut off cleanly charged %d, want the full hold %d", spent, hold)
	}
	spent, hold = callStrict(t, upstreamReturning(t, 500, "application/json", `{"usage":{"input_tokens":1,"output_tokens":1}}`))
	if spent != hold {
		t.Errorf("5xx charged %d, want the full hold %d", spent, hold)
	}
	spent, _ = callStrict(t, upstreamReturning(t, 400, "application/json", `{}`))
	if spent != 0 {
		t.Errorf("4xx charged %d, want 0", spent)
	}
}

func TestHopByHopHeadersDoNotCrossTheProxy(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	gw, err := New(Naive, upstream.URL, 1000, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("Proxy-Authorization", "secret")
	req.Header.Set("Te", "trailers")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	for _, name := range []string{"Upgrade", "Proxy-Authorization", "Te"} {
		if got.Get(name) != "" {
			t.Errorf("%s reached the provider", name)
		}
	}
	if rec.Header().Get("Upgrade") != "" {
		t.Error("Upgrade reached the client")
	}
}
