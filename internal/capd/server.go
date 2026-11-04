// Package capd is a small LLM gateway whose one job is to not exceed a budget. It
// holds the worst-case cost durably before forwarding a call, then settles the true
// cost, and it fails closed.
package capd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/meter"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

const (
	maxBody     = 32 << 20
	maxResponse = 512 << 20
	// Repetition detection only logs for now: no credible false-positive rate exists.
	repeatWindow    = 10
	repeatThreshold = 4
	maxTrackedRuns  = 10_000
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// hopByHop headers describe one connection and must not cross the proxy.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// ModelPrice is what a config file says about one model, in dollars per million tokens.
type ModelPrice struct {
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheWrite string `json:"cache_write,omitempty"`
	CacheRead  string `json:"cache_read,omitempty"`
}

// KeyConfig is one virtual key. Only its SHA-256 is stored.
type KeyConfig struct {
	Name         string `json:"name"`
	SHA256       string `json:"sha256"`
	BudgetUSD    string `json:"budget_usd"`
	RunBudgetUSD string `json:"run_budget_usd,omitempty"`
	MaxRunSteps  int64  `json:"max_run_steps,omitempty"`
	USDPerMinute string `json:"usd_per_minute,omitempty"`
}

// Config is the gateway's configuration. Upstream URLs live only here, never in a
// request, so a caller cannot point the gateway at another host.
type Config struct {
	Listen       string                `json:"listen"`
	DB           string                `json:"db"`
	Synchronous  string                `json:"synchronous"`
	OpenAIURL    string                `json:"openai_url"`
	AnthropicURL string                `json:"anthropic_url"`
	Keys         []KeyConfig           `json:"keys"`
	Prices       map[string]ModelPrice `json:"prices"`
	// BytesPerToken bounds input before a call: tokens cannot exceed bytes/this. The
	// default of 1 is provable and wasteful; raise it only with evidence.
	BytesPerToken int `json:"bytes_per_token,omitempty"`
}

type key struct {
	name      string
	budget    money.Pico
	runBudget money.Pico
	maxSteps  int64
	perMinute money.Pico
}

// Server is the gateway.
type Server struct {
	ledger        *Ledger
	openai        string
	anthropic     string
	openaiKey     string
	anthropicKey  string
	keys          map[string]key
	rates         map[string]meter.Rates
	bytesPerToken int64
	client        *http.Client
	hmacKey       []byte

	mu      sync.Mutex
	windows map[string]*spendWindow
	recent  map[string][]string
}

type spendWindow struct {
	minute int64
	spent  money.Pico
}

// LoadConfig reads and checks a config file.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // a misspelt budget field must not silently mean no budget
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// New builds a gateway on an open ledger. Provider keys come from the environment.
func New(cfg Config, ledger *Ledger) (*Server, error) {
	if cfg.OpenAIURL == "" && cfg.AnthropicURL == "" {
		return nil, errors.New("configure openai_url or anthropic_url")
	}
	for _, raw := range []string{cfg.OpenAIURL, cfg.AnthropicURL} {
		if raw != "" && !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
			return nil, fmt.Errorf("upstream must be an http(s) URL, got %q", raw)
		}
	}
	if len(cfg.Keys) == 0 || len(cfg.Prices) == 0 {
		return nil, errors.New("configure at least one key and one model price")
	}
	if cfg.BytesPerToken < 0 {
		return nil, errors.New("bytes_per_token must not be negative")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	s := &Server{
		ledger: ledger, openai: strings.TrimRight(cfg.OpenAIURL, "/"), anthropic: strings.TrimRight(cfg.AnthropicURL, "/"),
		openaiKey: os.Getenv("OPENAI_API_KEY"), anthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
		keys: map[string]key{}, rates: map[string]meter.Rates{}, bytesPerToken: max(1, int64(cfg.BytesPerToken)),
		hmacKey: secret, windows: map[string]*spendWindow{}, recent: map[string][]string{},
		client: &http.Client{
			Transport: &http.Transport{
				DisableCompression: true, MaxIdleConnsPerHost: 256,
				ResponseHeaderTimeout: 10 * time.Minute, MaxResponseHeaderBytes: 1 << 20,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	for _, k := range cfg.Keys {
		parsed, err := parseKey(k)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k.Name, err)
		}
		hash := strings.ToLower(k.SHA256)
		if decoded, err := hex.DecodeString(hash); err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("key %q: sha256 must be 64 hex characters", k.Name)
		}
		if _, dup := s.keys[hash]; dup {
			return nil, fmt.Errorf("key %q repeats another key's hash", k.Name)
		}
		s.keys[hash] = parsed
	}
	for model, p := range cfg.Prices {
		rates, err := parseRates(p)
		if err != nil {
			return nil, fmt.Errorf("price for %q: %w", model, err)
		}
		s.rates[model] = rates
	}
	return s, nil
}

func parseKey(k KeyConfig) (key, error) {
	out := key{name: k.Name, maxSteps: k.MaxRunSteps}
	var err error
	if k.Name == "" {
		return out, errors.New("a key needs a name")
	}
	if out.budget, err = money.ParseUSD(k.BudgetUSD); err != nil {
		return out, fmt.Errorf("budget_usd: %w", err)
	}
	if out.budget <= 0 {
		return out, errors.New("budget_usd must be above zero")
	}
	if k.RunBudgetUSD != "" {
		if out.runBudget, err = money.ParseUSD(k.RunBudgetUSD); err != nil {
			return out, fmt.Errorf("run_budget_usd: %w", err)
		}
	}
	if k.USDPerMinute != "" {
		if out.perMinute, err = money.ParseUSD(k.USDPerMinute); err != nil {
			return out, fmt.Errorf("usd_per_minute: %w", err)
		}
	}
	return out, nil
}

// parseRates fills cache rates conservatively when unset: writes at twice input,
// the most any published tier charges, so a missing price cannot undercount.
func parseRates(p ModelPrice) (meter.Rates, error) {
	var r meter.Rates
	var err error
	if r.Input, err = money.ParseRate(p.Input); err != nil {
		return r, err
	}
	if r.Output, err = money.ParseRate(p.Output); err != nil {
		return r, err
	}
	if r.Input <= 0 || r.Output <= 0 {
		return r, errors.New("a zero price would make every call free to the ledger")
	}
	r.CacheWrite, r.CacheRead = r.Input*2, r.Input
	if p.CacheWrite != "" {
		if r.CacheWrite, err = money.ParseRate(p.CacheWrite); err != nil {
			return r, err
		}
	}
	if p.CacheRead != "" {
		if r.CacheRead, err = money.ParseRate(p.CacheRead); err != nil {
			return r, err
		}
	}
	return r, nil
}

// HashKey returns the value to store in a config file for a virtual key.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	path, runID, ok := splitRun(r.URL.Path)
	if !ok {
		s.fail(w, r.URL.Path, http.StatusBadRequest, "invalid_run_id", "the run ID has unsupported characters or is too long")
		return
	}
	if header := r.Header.Get("X-Run-Id"); header != "" {
		if !runIDPattern.MatchString(header) {
			s.fail(w, path, http.StatusBadRequest, "invalid_run_id", "the run ID has unsupported characters or is too long")
			return
		}
		runID = header
	}
	k, ok := s.authenticate(r)
	if !ok {
		s.fail(w, path, http.StatusUnauthorized, "authentication_error", "unknown API key")
		return
	}

	// These routes cost nothing, and Claude Code relies on them.
	if (r.Method == http.MethodGet && path == "/v1/models") || (r.Method == http.MethodPost && path == "/v1/messages/count_tokens") {
		s.forwardFree(w, r, path)
		return
	}
	if r.Method != http.MethodPost || (path != "/v1/chat/completions" && path != "/v1/messages") {
		s.fail(w, path, http.StatusNotFound, "not_found", "unknown route")
		return
	}
	s.meteredCall(w, r, path, runID, k)
}

func splitRun(path string) (rest, runID string, ok bool) {
	after, found := strings.CutPrefix(path, "/r/")
	if !found {
		return path, "", true
	}
	runID, rest, found = strings.Cut(after, "/")
	if !found || !runIDPattern.MatchString(runID) {
		return path, "", false
	}
	return "/" + rest, runID, true
}

func (s *Server) authenticate(r *http.Request) (key, bool) {
	raw := r.Header.Get("x-api-key")
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		raw = bearer
	}
	if raw == "" {
		return key{}, false
	}
	// The lookup is on a hash, so its timing reveals nothing about the raw key.
	k, ok := s.keys[HashKey(raw)]
	return k, ok
}

func (s *Server) upstream(path string) (base, apiKey string) {
	if strings.HasPrefix(path, "/v1/messages") {
		return s.anthropic, s.anthropicKey
	}
	return s.openai, s.openaiKey
}

func (s *Server) meteredCall(w http.ResponseWriter, r *http.Request, path, runID string, k key) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(60 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		s.fail(w, path, http.StatusRequestEntityTooLarge, "invalid_request_error", "the body is too large or unreadable")
		return
	}
	call, err := meter.Inspect(body)
	if err != nil {
		s.fail(w, path, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	rates, known := s.rates[call.Model]
	if !known {
		// An unpriced model would be free to the ledger and very much not free to you.
		s.fail(w, path, http.StatusBadRequest, "unknown_model", "this model has no configured price, so it cannot be budgeted")
		return
	}
	base, apiKey := s.upstream(path)
	if base == "" {
		s.fail(w, path, http.StatusNotFound, "not_found", "no upstream is configured for this API")
		return
	}
	if path == "/v1/chat/completions" && call.Stream && !call.HasUsageOption {
		if body, err = requestUsage(body); err != nil {
			s.fail(w, path, http.StatusBadRequest, "invalid_request_error", "the request could not be read")
			return
		}
	}

	inputBound := (int64(len(body)) + s.bytesPerToken - 1) / s.bytesPerToken
	hold := money.Add(money.Mul(max(rates.Input, rates.CacheWrite), inputBound), money.Mul(rates.Output, call.MaxOutput))
	if !s.withinVelocity(k, hold) {
		s.fail(w, path, http.StatusTooManyRequests, "velocity_limit", "spend per minute is over this key's limit")
		return
	}

	scopes := []Scope{{ID: "key:" + k.name, Cap: k.budget}}
	if runID != "" {
		runCap := k.budget
		if k.runBudget > 0 {
			runCap = k.runBudget
		}
		scopes = append(scopes, Scope{ID: "run:" + k.name + ":" + runID, Cap: runCap, MaxSteps: k.maxSteps})
		s.noteRepeat(k.name, runID, body)
	}
	id, decision, err := s.ledger.Reserve(r.Context(), scopes, hold)
	if err != nil {
		log.Printf("key=%s reservation failed, refusing the call: %v", k.name, err)
		s.fail(w, path, http.StatusServiceUnavailable, "ledger_unavailable", "the budget ledger is unavailable, so the call was not sent")
		return
	}
	switch decision {
	case Exhausted:
		s.fail(w, path, http.StatusPaymentRequired, "budget_exceeded", "this budget is spent")
		return
	case HeldBack:
		w.Header().Set("Retry-After", "5")
		s.fail(w, path, http.StatusTooManyRequests, "budget_held", "in-flight calls hold the rest of this budget; retry shortly")
		return
	case StepLimit:
		s.fail(w, path, http.StatusTooManyRequests, "step_limit", "this run has made too many calls")
		return
	}

	// Settlement must survive the client leaving, and a panic must not strand a hold.
	settled := false
	settle := func(apply func(context.Context) error) {
		settled = true
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if err := apply(ctx); err != nil {
			// The hold stays open, so the budget stays reserved until a restart captures it.
			log.Printf("key=%s hold=%d settlement failed: %v", k.name, id, err)
		}
	}
	defer func() {
		if !settled {
			settle(func(ctx context.Context) error { return s.ledger.Capture(ctx, id) })
		}
	}()

	outcome, cost := s.relay(w, r, path, base, apiKey, body, rates)
	switch outcome {
	case unbilled:
		settle(func(ctx context.Context) error { return s.ledger.Release(ctx, id) })
	case costKnown:
		if cost > hold {
			log.Printf("OVERAGE key=%s hold=%d cost %s exceeded the %s held; the provider broke an assumption", k.name, id, cost, hold)
		}
		settle(func(ctx context.Context) error { return s.ledger.Settle(ctx, id, cost) })
	default:
		settle(func(ctx context.Context) error { return s.ledger.Capture(ctx, id) })
	}
}

func requestUsage(body []byte) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	return json.Marshal(fields)
}

type relayOutcome int

const (
	costUnknown relayOutcome = iota
	costKnown
	unbilled
)

func (s *Server) newUpstreamRequest(r *http.Request, url, apiKey string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	// Removing these, not filtering them, is what keeps the virtual key from leaking.
	for _, name := range append([]string{"Authorization", "X-Api-Key", "X-Run-Id", "Cookie", "Accept-Encoding", "Forwarded", "X-Forwarded-For"}, hopByHop...) {
		req.Header.Del(name)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}
	return req, nil
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	for _, name := range hopByHop {
		w.Header().Del(name)
	}
}

func (s *Server) relay(w http.ResponseWriter, r *http.Request, path, base, apiKey string, body []byte, rates meter.Rates) (relayOutcome, money.Pico) {
	req, err := s.newUpstreamRequest(r, base+path, apiKey, bytes.NewReader(body))
	if err != nil {
		s.fail(w, path, http.StatusBadGateway, "upstream_error", "the upstream request could not be built")
		return unbilled, 0
	}
	resp, err := s.client.Do(req)
	if err != nil {
		// The call may have reached the provider before failing, so the hold stands.
		if !errors.Is(err, context.Canceled) {
			log.Printf("upstream call failed: %v", err)
			s.fail(w, path, http.StatusBadGateway, "upstream_error", "the provider could not be reached")
		}
		return costUnknown, 0
	}
	defer resp.Body.Close()
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)

	limited := &io.LimitedReader{R: resp.Body, N: maxResponse}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(w, limited)
		if resp.StatusCode >= 300 && resp.StatusCode < 500 {
			return unbilled, 0
		}
		return costUnknown, 0 // a 5xx may have failed after generating output
	}

	tracker := meter.Tracker{Rates: rates}
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if !isStream {
		raw, err := io.ReadAll(limited)
		if err != nil || limited.N <= 0 {
			return costUnknown, 0
		}
		if _, err := w.Write(raw); err != nil {
			return costUnknown, 0
		}
		tracker.Feed(raw)
		if cost, known := tracker.Cost(); known {
			return costKnown, cost
		}
		return costUnknown, 0
	}

	rc := http.NewResponseController(w)
	reader := bufio.NewReader(limited)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				tracker.Feed(data)
			}
			if _, err := w.Write(line); err != nil {
				return costUnknown, 0
			}
			if err := rc.Flush(); err != nil {
				return costUnknown, 0
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) || limited.N <= 0 || !tracker.Finished() || r.Context().Err() != nil {
				return costUnknown, 0
			}
			break
		}
	}
	if cost, known := tracker.Cost(); known {
		return costKnown, cost
	}
	return costUnknown, 0
}

func (s *Server) forwardFree(w http.ResponseWriter, r *http.Request, path string) {
	base, apiKey := s.upstream(path)
	if base == "" {
		s.fail(w, path, http.StatusNotFound, "not_found", "no upstream is configured for this API")
		return
	}
	req, err := s.newUpstreamRequest(r, base+path, apiKey, http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		s.fail(w, path, http.StatusBadGateway, "upstream_error", "the upstream request could not be built")
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.fail(w, path, http.StatusBadGateway, "upstream_error", "the provider could not be reached")
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxResponse))
}

// withinVelocity is the fast brake on a runaway. It is counted in memory, since the
// durable budget is the real limit and this only needs to act within the minute.
func (s *Server) withinVelocity(k key, hold money.Pico) bool {
	if k.perMinute <= 0 {
		return true
	}
	minute := time.Now().Unix() / 60
	s.mu.Lock()
	defer s.mu.Unlock()
	window := s.windows[k.name]
	if window == nil || window.minute != minute {
		window = &spendWindow{minute: minute}
		s.windows[k.name] = window
	}
	if money.Add(window.spent, hold) > k.perMinute {
		return false
	}
	window.spent = money.Add(window.spent, hold)
	return true
}

// noteRepeat logs a run sending the same latest message again and again, which is
// what a stuck tool loop looks like at a proxy: whole prompts differ as history
// grows, but the newest message repeats. Only an HMAC is kept, never content.
func (s *Server) noteRepeat(keyName, runID string, body []byte) {
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Messages) == 0 {
		return
	}
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write(doc.Messages[len(doc.Messages)-1])
	sum := hex.EncodeToString(mac.Sum(nil))
	id := keyName + ":" + runID

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recent) >= maxTrackedRuns && s.recent[id] == nil {
		clear(s.recent) // shortcut: drops all history at the limit, use an LRU if logs prove useful
	}
	window := append(s.recent[id], sum)
	if len(window) > repeatWindow {
		window = window[len(window)-repeatWindow:]
	}
	s.recent[id] = window
	repeats := 0
	for _, seen := range window {
		if seen == sum {
			repeats++
		}
	}
	if repeats == repeatThreshold {
		log.Printf("LOOP? key=%s run=%s sent the same latest message %d times in %d calls", keyName, runID, repeats, len(window))
	}
}

func (s *Server) fail(w http.ResponseWriter, path string, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body any = map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}}
	if strings.HasPrefix(path, "/v1/messages") {
		body = map[string]any{"type": "error", "error": map[string]any{"type": code, "message": message}}
	}
	_ = json.NewEncoder(w).Encode(body)
}
