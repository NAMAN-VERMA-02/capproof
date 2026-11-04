// Package control holds two tiny gateways that calibrate the test suite. Naive
// counts spend after each response, the design real gateways commonly use, and
// must fail. Strict holds the worst case before forwarding and must pass. A suite
// that cannot tell them apart proves nothing about anything else.
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

// Modes.
const (
	Naive  = "naive"
	Strict = "strict"
)

const (
	maxBody     = 8 << 20
	maxResponse = 64 << 20
)

// hopByHop headers describe one connection and must not cross the proxy.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Gateway is one control gateway with a single shared budget.
type Gateway struct {
	mode       string
	upstream   string
	cap        money.Pico
	inputRate  money.Pico
	outputRate money.Pico
	client     *http.Client

	mu    sync.Mutex
	spent money.Pico
	held  money.Pico
}

// New returns a Gateway. Upstream comes only from this configuration, never from
// a request, so a caller cannot point the gateway at another host.
func New(mode, upstream string, limit, inputRate, outputRate money.Pico) (*Gateway, error) {
	if mode != Naive && mode != Strict {
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("upstream must be an http(s) URL, got %q", upstream)
	}
	if limit <= 0 {
		return nil, errors.New("the cap must be above zero")
	}
	return &Gateway{
		mode: mode, upstream: strings.TrimRight(upstream, "/"),
		cap: limit, inputRate: inputRate, outputRate: outputRate,
		client: &http.Client{
			Transport:     &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 256},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Ledger returns the gateway's own accounting.
func (g *Gateway) Ledger() (spent, held money.Pico) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spent, g.held
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/messages") {
		http.NotFound(w, r)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	var hold money.Pico
	if g.mode == Strict {
		maxTokens, err := outputLimit(body)
		if err != nil {
			refuse(w, http.StatusBadRequest, err.Error())
			return
		}
		// Each input token is at least four bytes for the mock provider, so a
		// count over the whole body can only overestimate.
		inputBound := int64(len(body)+3) / 4
		hold = money.Add(money.Mul(g.inputRate, inputBound), money.Mul(g.outputRate, maxTokens))
	}
	if !g.admit(hold) {
		refuse(w, http.StatusTooManyRequests, "budget exceeded")
		return
	}

	cost, known, sent := g.forward(w, r, body)
	g.settle(hold, cost, known, sent)
}

// outputLimit returns the most output a call can be billed for, or an error when
// that cannot be known. It guards against parser differentials: this gateway and
// the provider must agree on the limit, yet they may treat duplicate keys, letter
// case and the two field names differently, and each gap is a way to hold for one
// token and be billed for thousands.
func outputLimit(body []byte) (int64, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, errors.New("the request must be a JSON object")
	}
	seen := map[string]bool{}
	limit := int64(0)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return 0, errors.New("the request is not valid JSON")
		}
		key := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return 0, errors.New("the request is not valid JSON")
		}
		lower := strings.ToLower(key)
		if lower != "max_tokens" && lower != "max_completion_tokens" && lower != "n" {
			continue
		}
		if lower != key || seen[lower] {
			return 0, fmt.Errorf("the %q field is repeated or wrongly cased, so the limit is ambiguous", key)
		}
		seen[lower] = true
		var n int64
		if err := json.Unmarshal(value, &n); err != nil || n < 1 {
			return 0, fmt.Errorf("%s must be a positive integer", lower)
		}
		if lower == "n" {
			if n != 1 {
				return 0, errors.New("only n=1 can be held for")
			}
			continue
		}
		limit = max(limit, n)
	}
	if limit == 0 {
		// Without an output limit the worst case is unbounded, so no hold can be sound.
		return 0, errors.New("a positive max_tokens is required to bound the cost")
	}
	return limit, nil
}

func refuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": "budget_exceeded"}})
}

func (g *Gateway) admit(hold money.Pico) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode == Naive {
		return g.spent < g.cap
	}
	if money.Add(money.Add(g.spent, g.held), hold) > g.cap {
		return false
	}
	g.held = money.Add(g.held, hold)
	return true
}

// settle closes one call. In Strict mode a call whose cost is unknown keeps its
// full hold, unlike a card payment that lapses, because the provider may already
// have billed. Naive records nothing it cannot see, which is the bug it models.
func (g *Gateway) settle(hold, cost money.Pico, known, sent bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode == Naive {
		if known {
			g.spent = money.Add(g.spent, cost)
		}
		return
	}
	g.held -= hold
	switch {
	case !sent:
		// The provider refused or was never reached, so nothing was billed.
	case known:
		g.spent = money.Add(g.spent, cost)
	default:
		g.spent = money.Add(g.spent, hold)
	}
}

// forward relays the call. known reports that the response ended cleanly with
// usable usage; sent reports that the provider may have billed for it.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, body []byte) (cost money.Pico, known, sent bool) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.upstream+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusBadGateway)
		return 0, false, false
	}
	req.Header = r.Header.Clone()
	for _, name := range hopByHop {
		req.Header.Del(name)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		// The call may have reached the provider before failing, so treat it as billable.
		if !errors.Is(err, context.Canceled) {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
		return 0, false, true
	}
	defer resp.Body.Close()

	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	for _, name := range hopByHop {
		w.Header().Del(name)
	}
	w.WriteHeader(resp.StatusCode)
	limited := &io.LimitedReader{R: resp.Body, N: maxResponse}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(w, limited)
		// A 4xx was refused unbilled. A 5xx may have failed after generating output.
		return 0, false, resp.StatusCode >= 500
	}

	rc := http.NewResponseController(w)
	tracker := usageTracker{inputRate: g.inputRate, outputRate: g.outputRate}
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	reader := bufio.NewReader(limited)
	complete := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if isStream {
				if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
					tracker.feed(data)
				}
			} else {
				tracker.feed(line)
			}
			if _, err := w.Write(line); err != nil {
				return 0, false, true
			}
			if err := rc.Flush(); err != nil {
				return 0, false, true
			}
		}
		if readErr != nil {
			complete = errors.Is(readErr, io.EOF) && limited.N > 0
			break
		}
	}
	if !complete || r.Context().Err() != nil {
		return 0, false, true
	}
	// A stream cut off cleanly still reads as EOF, with usage that stops early.
	if isStream && !tracker.finished {
		return 0, false, true
	}
	cost, known = tracker.cost()
	return cost, known, true
}

// usageTracker keeps the latest usage seen in a response. Streams report usage
// cumulatively, so the last value wins, and malformed usage makes the cost unknown.
type usageTracker struct {
	inputRate, outputRate money.Pico
	input, output         *int64
	malformed             bool
	finished              bool
}

func (u *usageTracker) feed(data []byte) {
	data = bytes.TrimSpace(data)
	if string(data) == "[DONE]" {
		u.finished = true
		return
	}
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var doc struct {
		Type    string          `json:"type"`
		Usage   json.RawMessage `json:"usage"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return
	}
	if doc.Type == "message_stop" {
		u.finished = true
	}
	for _, raw := range []json.RawMessage{doc.Usage, doc.Message.Usage} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var fields struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			InputTokens      *int64 `json:"input_tokens"`
			OutputTokens     *int64 `json:"output_tokens"`
		}
		if json.Unmarshal(raw, &fields) != nil {
			u.malformed = true
			continue
		}
		for _, in := range []*int64{fields.PromptTokens, fields.InputTokens} {
			if in != nil {
				u.input = in
			}
		}
		for _, out := range []*int64{fields.CompletionTokens, fields.OutputTokens} {
			if out != nil {
				u.output = out
			}
		}
	}
}

func (u *usageTracker) cost() (money.Pico, bool) {
	if u.malformed || u.input == nil || u.output == nil || *u.input < 0 || *u.output < 0 {
		return 0, false
	}
	return money.Add(money.Mul(u.inputRate, *u.input), money.Mul(u.outputRate, *u.output)), true
}
