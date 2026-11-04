package capd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/faultproxy"
	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

// TestChaos throws random faults at the gateway and checks the invariants after
// every seed. Set CAPPROOF_CHAOS_SEEDS to run more than CI's quick pass.
func TestChaos(t *testing.T) {
	log.SetOutput(io.Discard) // injected faults make every seed noisy
	defer log.SetOutput(os.Stderr)
	seeds := 25
	if raw := os.Getenv("CAPPROOF_CHAOS_SEEDS"); raw != "" {
		var err error
		if seeds, err = strconv.Atoi(raw); err != nil {
			t.Fatal(err)
		}
	}
	first := uint64(1)
	if raw := os.Getenv("CAPPROOF_CHAOS_FIRST_SEED"); raw != "" {
		var err error
		if first, err = strconv.ParseUint(raw, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	var calls, refused, cancelled atomic.Int64
	start := time.Now()
	for seed := first; seed < first+uint64(seeds); seed++ {
		if !chaosRun(t, seed, &calls, &refused, &cancelled) {
			return
		}
	}
	t.Logf("%d seeds, %d calls, %d refused, %d cancelled, 0 violations in %s", seeds, calls.Load(), refused.Load(), cancelled.Load(), time.Since(start).Round(time.Second))
}

func chaosRun(t *testing.T, seed uint64, calls, refused, cancelled *atomic.Int64) bool {
	t.Helper()
	fail := func(format string, args ...any) bool {
		t.Errorf("seed %d: %s", seed, fmt.Sprintf(format, args...))
		return false
	}
	rng := rand.New(rand.NewPCG(seed, 42))
	cfg := mock.DefaultConfig()
	cfg.LatencyMS, cfg.ChunkDelayMS = rng.IntN(4), rng.IntN(2)
	cfg.OutputTokens = 100 + rng.IntN(400)
	cfg.OnDisconnect = []string{"full", "delivered"}[rng.IntN(2)]
	cfg.Usage = []string{"normal", "normal", "omit", "malformed"}[rng.IntN(4)]
	provider, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(provider.Handler())
	defer upstream.Close()
	proxy, err := faultproxy.Start(strings.TrimPrefix(upstream.URL, "http://"), seed)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	dir, err := os.MkdirTemp("", "capd-chaos")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ledger, _, err := Open(filepath.Join(dir, "l.db"), "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()

	// A few calls' worth, varied so the last admission lands differently each time.
	limit := money.Pico(300_000_000 + rng.Int64N(1_500_000_000))
	url := "http://" + proxy.Addr()
	server, err := New(Config{
		OpenAIURL: url, AnthropicURL: url,
		Keys:   []KeyConfig{{Name: "ci", SHA256: HashKey(testKey), BudgetUSD: limit.Dollars()}},
		Prices: map[string]ModelPrice{"m": {Input: "0.15", Output: "0.60", CacheWrite: "0.15"}},
	}, ledger)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(server)
	defer front.Close()

	type plan struct {
		path    string
		body    string
		timeout time.Duration
	}
	plans := make([]plan, 6+rng.IntN(20))
	for i := range plans {
		path, stream := "/v1/chat/completions", rng.IntN(2) == 0
		if rng.IntN(2) == 0 {
			path = "/v1/messages"
		}
		raw, _ := json.Marshal(map[string]any{
			"model": "m", "max_tokens": 100 + rng.IntN(500), "stream": stream,
			"messages": []map[string]string{{"role": "user", "content": strings.Repeat("x", 50+rng.IntN(800))}},
		})
		// Short deadlines cut calls off at random points; the long one ends stalls.
		timeout := time.Second
		if rng.IntN(3) == 0 {
			timeout = time.Duration(1+rng.IntN(30)) * time.Millisecond
		}
		plans[i] = plan{path: path, body: string(raw), timeout: timeout}
	}

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	var wg sync.WaitGroup
	for _, p := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			calls.Add(1)
			ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+p.path, strings.NewReader(p.body))
			req.Header.Set("Authorization", "Bearer "+testKey)
			resp, err := client.Do(req)
			if err != nil {
				cancelled.Add(1)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusTooManyRequests {
				refused.Add(1)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
	}
	wg.Wait()

	// Everything must come to rest: the provider idle and every hold resolved.
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	var b Balance
	for {
		if b, err = ledger.Balance(ctx, "key:ci"); err != nil {
			return fail("reading the ledger: %v", err)
		}
		if b.Held == 0 && provider.Snapshot().InFlight == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fail("I4: %s still held and %d calls in flight", b.Held, provider.Snapshot().InFlight)
		}
		time.Sleep(2 * time.Millisecond)
	}
	billed := provider.Snapshot().Billed
	switch {
	case b.Spent > limit:
		return fail("I1: spent %s past the %s cap", b.Spent, limit)
	case billed > limit:
		return fail("I2: provider billed %s past the %s cap", billed, limit)
	case b.Spent < billed:
		return fail("I3: ledger %s below the bill %s", b.Spent, billed)
	}

	// I5: nothing phantom is left behind. A call that fits must be admitted.
	if room := limit - b.Spent; room > 0 {
		id, decision, err := ledger.Reserve(ctx, []Scope{{ID: "key:ci", Cap: limit}}, room)
		if err != nil || decision != Admitted {
			return fail("I5: %s of room but decision %v, %v", room, decision, err)
		}
		if err := ledger.Release(ctx, id); err != nil {
			return fail("releasing: %v", err)
		}
	}
	return true
}
