// Package mock is a fake LLM provider that doubles as the billing oracle. It
// speaks OpenAI Chat Completions and Anthropic Messages, and it records what a
// real provider would have charged, independently of whatever a gateway in
// front of it believes.
package mock

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

const (
	maxBody = 8 << 20
	// A cap on concurrent calls keeps a flood from exhausting memory, since each
	// call in "full" mode keeps running after its client has gone.
	defaultMaxInFlight = 4096
	bodyReadTimeout    = 30 * time.Second
	// TokenHeader carries the oracle token. Only the runner holds it.
	TokenHeader = "X-Capproof-Oracle-Token"
)

// Config controls pricing, pacing and the faults the provider injects.
type Config struct {
	InputRate    money.Pico `json:"input_rate_pico_per_token"`
	OutputRate   money.Pico `json:"output_rate_pico_per_token"`
	OutputTokens int        `json:"output_tokens"`
	Chunks       int        `json:"chunks"`
	LatencyMS    int        `json:"latency_ms"`
	ChunkDelayMS int        `json:"chunk_delay_ms"`
	// OnDisconnect is "full" when generation and billing continue after the
	// client leaves, or "delivered" when they stop. Providers have not published
	// which applies, so "full" is the default: it is the case that hurts.
	OnDisconnect string `json:"on_disconnect"`
	// Usage is "normal", "omit" or "malformed", and affects only what the
	// response reports. The oracle always bills the true amount.
	Usage string `json:"usage"`
	// IgnoreMaxTokens generates OutputTokens even past the request's limit.
	IgnoreMaxTokens bool `json:"ignore_max_tokens"`
	// RunID marks calls that belong to this run. Every prompt contains it, so a
	// call without it is a late retry from an earlier run or was invented by the
	// gateway, and the bill can no longer be attributed to this run.
	RunID string `json:"run_id,omitempty"`
}

// DefaultConfig prices calls like gpt-4o-mini ($0.15/$0.60 per million tokens).
// The one-second latency keeps every call of a burst in flight at once.
func DefaultConfig() Config {
	return Config{
		InputRate:    150_000,
		OutputRate:   600_000,
		OutputTokens: 500,
		Chunks:       5,
		LatencyMS:    1000,
		ChunkDelayMS: 50,
		OnDisconnect: "full",
		Usage:        "normal",
	}
}

// Validate rejects settings that would make results meaningless.
func (c Config) Validate() error {
	switch {
	case c.InputRate < 0 || c.OutputRate < 0:
		return errors.New("rates must not be negative")
	case c.OutputTokens < 1 || c.OutputTokens > 1_000_000:
		return errors.New("output_tokens must be between 1 and 1000000")
	case c.Chunks < 1 || c.Chunks > 1000:
		return errors.New("chunks must be between 1 and 1000")
	case c.LatencyMS < 0 || c.LatencyMS > 600_000:
		return errors.New("latency_ms must be between 0 and 600000")
	case c.ChunkDelayMS < 0 || c.ChunkDelayMS > 60_000:
		return errors.New("chunk_delay_ms must be between 0 and 60000")
	case c.OnDisconnect != "full" && c.OnDisconnect != "delivered":
		return errors.New(`on_disconnect must be "full" or "delivered"`)
	case c.Usage != "normal" && c.Usage != "omit" && c.Usage != "malformed":
		return errors.New(`usage must be "normal", "omit" or "malformed"`)
	}
	return nil
}

// Snapshot is one consistent reading of the oracle.
type Snapshot struct {
	Requests     int64      `json:"requests"`
	Rejected     int64      `json:"rejected"`
	Overloaded   int64      `json:"overloaded"`
	Foreign      int64      `json:"foreign_calls"`
	Unrouted     int64      `json:"unrouted"`
	LastUnrouted string     `json:"last_unrouted,omitempty"`
	InFlight     int64      `json:"in_flight"`
	MaxInFlight  int64      `json:"max_in_flight"`
	Disconnects  int64      `json:"disconnects"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	Billed       money.Pico `json:"billed_pico"`
	BilledUSD    float64    `json:"billed_usd"`
	Config       Config     `json:"config"`
}

// Server is the provider and the oracle.
type Server struct {
	defaults Config
	seq      atomic.Int64
	// OracleToken, when set, is required on every oracle route. The gateway under
	// test shares a network with this server and is untrusted: without a token it
	// could reset the ledger mid-run and turn a breach into a pass.
	OracleToken string
	maxInFlight int64

	mu  sync.Mutex
	cfg Config
	s   Snapshot
}

// New returns a Server, or an error if the configuration is invalid.
func New(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Server{defaults: cfg, cfg: cfg, maxInFlight: defaultMaxInFlight}, nil
}

// Handler routes provider and oracle requests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Gateways differ on whether their base URL includes /v1, so accept both.
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /chat/completions", s.chat)
	mux.HandleFunc("POST /v1/messages", s.messages)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /models", s.models)
	mux.HandleFunc("GET /_oracle", s.admin(s.oracle))
	mux.HandleFunc("POST /_oracle/reset", s.admin(s.reset))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Recording misses matters: a gateway calling an unexpected route bills
	// nothing here, which would otherwise look like a cap that held.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.s.Unrouted++
		s.s.LastUnrouted = truncate(r.Method+" "+r.URL.Path, 200)
		s.mu.Unlock()
		http.NotFound(w, r)
	})
	return mux
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.OracleToken != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(TokenHeader)), []byte(s.OracleToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the oracle token is missing or wrong"})
			return
		}
		next(w, r)
	}
}

func truncate(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}

// Snapshot returns the current oracle reading.
func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Server) snapshotLocked() Snapshot {
	out := s.s
	out.BilledUSD = out.Billed.USD()
	out.Config = s.cfg
	return out
}

func (s *Server) oracle(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

// reset zeroes the ledger and applies overrides on top of the startup defaults,
// never the previous run's settings, so one run's faults cannot leak into the
// next. It refuses while calls are in flight, since they would bill into the
// new run.
func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	next := s.defaults
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := next.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.s.InFlight > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("%d calls still in flight", s.s.InFlight)})
		return
	}
	s.cfg = next
	s.s = Snapshot{}
	writeJSON(w, http.StatusOK, s.snapshotLocked())
}

func (s *Server) config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// begin admits one call and bills its input. It reports false when the provider
// is overloaded, in which case nothing is billed and end must not be called.
func (s *Server) begin(cfg Config, input int, body []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.s.InFlight >= s.maxInFlight {
		s.s.Overloaded++
		return false
	}
	if cfg.RunID != "" && !bytes.Contains(body, []byte(cfg.RunID)) {
		s.s.Foreign++
	}
	s.s.Requests++
	s.s.InFlight++
	s.s.MaxInFlight = max(s.s.MaxInFlight, s.s.InFlight)
	s.s.InputTokens += int64(input)
	s.s.Billed = money.Add(s.s.Billed, money.Mul(cfg.InputRate, int64(input)))
	return true
}

func (s *Server) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.InFlight--
}

func (s *Server) billOutput(cfg Config, tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.OutputTokens += int64(tokens)
	s.s.Billed = money.Add(s.s.Billed, money.Mul(cfg.OutputRate, int64(tokens)))
}

func (s *Server) noteDisconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.Disconnects++
}

func (s *Server) reject(w http.ResponseWriter, status int, body any) {
	s.mu.Lock()
	s.s.Rejected++
	s.mu.Unlock()
	writeJSON(w, status, body)
}

// generate bills output tokens as they are produced and passes each batch to
// emit. It reports whether the client was still connected at the end. In "full"
// mode it keeps generating and billing after the client leaves, ignoring the
// cancelled context, so every wait uses a plain timer from then on.
func (s *Server) generate(ctx context.Context, cfg Config, output, chunks int, emit func(tokens int) error) bool {
	gone := false
	leave := func() {
		if !gone {
			gone = true
			s.noteDisconnect()
		}
	}
	wait := func(d time.Duration) {
		if d <= 0 {
			return
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		if gone {
			<-timer.C
			return
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			leave()
			<-timer.C
		}
	}

	wait(time.Duration(cfg.LatencyMS) * time.Millisecond)
	chunks = min(chunks, output)
	per, extra := output/chunks, output%chunks
	for i := range chunks {
		if ctx.Err() != nil {
			leave()
		}
		if gone && cfg.OnDisconnect == "delivered" {
			return false
		}
		tokens := per
		if i < extra {
			tokens++
		}
		s.billOutput(cfg, tokens)
		if !gone {
			if err := emit(tokens); err != nil {
				leave()
			}
		}
		if i < chunks-1 {
			wait(time.Duration(cfg.ChunkDelayMS) * time.Millisecond)
		}
	}
	return !gone
}

// InputTokens is the deterministic stand-in for a tokenizer: one token per four
// bytes of the raw parts, rounded up. Using raw bytes means any content shape
// counts without parsing it.
func InputTokens(parts ...[]byte) int {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	return max(1, (n+3)/4)
}

func outputTokens(cfg Config, limit int) (tokens int, truncated bool) {
	if !cfg.IgnoreMaxTokens && limit > 0 && limit < cfg.OutputTokens {
		return limit, true
	}
	return cfg.OutputTokens, false
}

func text(tokens int) string {
	return strings.Repeat("tok ", tokens)
}

// readBody reads a size-limited body, writing the error itself on failure.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, fail func(string) any) ([]byte, bool) {
	// Only the body gets a deadline. A server-wide read timeout would also fire on
	// the disconnect watcher and make long streams look abandoned.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.reject(w, http.StatusRequestEntityTooLarge, fail("request body too large"))
		} else {
			s.reject(w, http.StatusBadRequest, fail("could not read request body"))
		}
		return nil, false
	}
	return body, true
}

func isNonEmptyArray(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 2 && raw[0] == '[' && !bytes.Equal(bytes.Join(bytes.Fields(raw), nil), []byte("[]"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sse writes and flushes one event. An empty event name is OpenAI's framing.
func sse(w io.Writer, rc *http.ResponseController, event string, payload any) error {
	data, ok := payload.(string)
	if !ok {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		data = string(raw)
	}
	var err error
	if event != "" {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	} else {
		_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	if err != nil {
		return err
	}
	return rc.Flush()
}

func setStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": "gpt-4o-mini", "object": "model", "owned_by": "capproof-mock"}},
	})
}
