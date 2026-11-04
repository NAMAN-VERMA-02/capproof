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

	"github.com/NAMAN-VERMA-02/capproof/internal/meter"
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
		call, err := meter.Inspect(body)
		if err != nil {
			refuse(w, http.StatusBadRequest, err.Error())
			return
		}
		// Each input token is at least four bytes for the mock provider, so a
		// count over the whole body can only overestimate.
		inputBound := int64(len(body)+3) / 4
		hold = money.Add(money.Mul(g.inputRate, inputBound), money.Mul(g.outputRate, call.MaxOutput))
	}
	if !g.admit(hold) {
		refuse(w, http.StatusTooManyRequests, "budget exceeded")
		return
	}

	cost, known, sent := g.forward(w, r, body)
	g.settle(hold, cost, known, sent)
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
	tracker := meter.Tracker{Rates: meter.Rates{Input: g.inputRate, Output: g.outputRate}}
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	reader := bufio.NewReader(limited)
	complete := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if isStream {
				if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
					tracker.Feed(data)
				}
			} else {
				tracker.Feed(line)
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
	if isStream && !tracker.Finished() {
		return 0, false, true
	}
	cost, known = tracker.Cost()
	return cost, known, true
}
