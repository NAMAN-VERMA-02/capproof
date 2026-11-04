// Package runner sends a scenario through a gateway and judges the result using
// only the oracle's bill. Nothing the gateway reports about itself is trusted.
package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

// Verdicts. INCONCLUSIVE exists so that a broken setup can never read as a cap
// that held.
const (
	Held         = "HELD"
	Breached     = "BREACHED"
	Inconclusive = "INCONCLUSIVE"
)

// Scenarios.
const (
	Burst      = "burst"      // all calls released at once
	Sequential = "sequential" // one call at a time
	Disconnect = "disconnect" // streams dropped by the client after the first byte
)

// Options describes one run. Cap must match the budget configured on the gateway.
type Options struct {
	Scenario    string
	Target      string
	Oracle      string
	Format      string // "openai" or "anthropic"
	Model       string
	APIKey      string
	Headers     []string // "Name: value"; {run} expands to the run ID
	Cap         money.Pico
	Requests    int
	Concurrency int
	Wave2       int           // calls sent after the settle wait, to catch late accounting
	Settle      time.Duration // wait before Wave2, past any batched spend writes
	MaxTokens   int
	PromptBytes int
	Stream      bool
	Timeout     time.Duration // per call
	Drain       time.Duration // how long the provider may take to go idle
	RunID       string        // generated when empty
	Mock        json.RawMessage
}

// Report is the result of one run.
type Report struct {
	Scenario        string            `json:"scenario"`
	Target          string            `json:"target"`
	Format          string            `json:"format"`
	Model           string            `json:"model"`
	Stream          bool              `json:"stream"`
	RunID           string            `json:"run_id"`
	Verdict         string            `json:"verdict"`
	Reasons         []string          `json:"reasons"`
	Warnings        []string          `json:"warnings,omitempty"`
	Cap             money.Pico        `json:"cap_pico"`
	CapUSD          float64           `json:"cap_usd"`
	Billed          money.Pico        `json:"billed_pico"`
	BilledUSD       float64           `json:"billed_usd"`
	Overshoot       money.Pico        `json:"overshoot_pico"`
	OvershootPct    float64           `json:"overshoot_pct"`
	UsablePct       float64           `json:"usable_budget_pct"`
	CallCost        money.Pico        `json:"call_cost_pico"`
	CallsCapAllows  int64             `json:"calls_cap_allows"`
	Sent            int               `json:"sent"`
	Admitted        int               `json:"admitted"`
	Statuses        map[string]int    `json:"statuses"`
	TransportErrors int               `json:"transport_errors"`
	BrokenStreams   int               `json:"broken_streams"`
	Samples         map[string]string `json:"samples,omitempty"`
	Provider        mock.Snapshot     `json:"provider"`
	StartedAt       time.Time         `json:"started_at"`
	DurationMS      int64             `json:"duration_ms"`
}

func (o *Options) setDefaults() {
	if o.Format == "" {
		o.Format = "openai"
	}
	if o.Model == "" {
		o.Model = "gpt-4o-mini"
	}
	if o.Requests == 0 {
		o.Requests = 30
	}
	if o.MaxTokens == 0 {
		o.MaxTokens = 500
	}
	if o.PromptBytes == 0 {
		o.PromptBytes = 4000
	}
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	if o.Drain == 0 {
		o.Drain = 60 * time.Second
	}
	if o.Concurrency == 0 {
		o.Concurrency = 1
		if o.Scenario == Burst {
			o.Concurrency = o.Requests
		}
	}
	if o.Scenario == Disconnect {
		o.Stream = true
	}
}

func (o *Options) validate() error {
	switch o.Scenario {
	case Burst, Sequential, Disconnect:
	default:
		return fmt.Errorf("unknown scenario %q", o.Scenario)
	}
	if o.Format != "openai" && o.Format != "anthropic" {
		return fmt.Errorf("unknown format %q", o.Format)
	}
	for name, raw := range map[string]string{"target": o.Target, "oracle": o.Oracle} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s must be an http(s) URL, got %q", name, raw)
		}
	}
	switch {
	case o.Cap <= 0:
		return errors.New("the cap must be above zero")
	case o.Requests < 1:
		return errors.New("requests must be at least 1")
	case o.Concurrency < 1 || o.Concurrency > o.Requests:
		return fmt.Errorf("concurrency must be between 1 and requests (%d)", o.Requests)
	case o.Scenario == Sequential && o.Concurrency != 1:
		return errors.New("the sequential scenario runs with concurrency 1")
	case o.Wave2 < 0 || o.Settle < 0:
		return errors.New("wave2 and settle must not be negative")
	case o.MaxTokens < 1 || o.PromptBytes < 64 || o.PromptBytes > 4<<20:
		return errors.New("max tokens must be at least 1 and the prompt between 64 bytes and 4 MiB")
	case o.Timeout <= 0 || o.Drain <= 0:
		return errors.New("timeouts must be above zero")
	}
	for _, h := range o.Headers {
		name, _, ok := strings.Cut(h, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return fmt.Errorf("header %q must look like Name: value", h)
		}
		if strings.EqualFold(name, "host") {
			return errors.New("set the Host through the target URL, not a header")
		}
	}
	return nil
}

type run struct {
	o       Options
	client  *http.Client
	url     string
	headers http.Header

	mu sync.Mutex
	r  *Report
}

// Run executes the scenario. It returns an error only when the test could not be
// set up or read; every outcome of the gateway itself becomes a Report.
func Run(ctx context.Context, o Options) (*Report, error) {
	o.setDefaults()
	if err := o.validate(); err != nil {
		return nil, err
	}
	if o.RunID == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		o.RunID = "capproof-" + hex.EncodeToString(b[:])
	}

	r := &run{
		o: o,
		client: &http.Client{
			// A direct transport: an inherited proxy setting would silently put
			// another hop between the test and the gateway.
			Transport: &http.Transport{
				MaxIdleConnsPerHost: o.Concurrency,
				DisableCompression:  true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		url:     endpoint(o.Target, o.Format),
		headers: buildHeaders(o),
		r: &Report{
			Scenario: o.Scenario, Target: o.Target, Format: o.Format, Model: o.Model, Stream: o.Stream,
			RunID: o.RunID, Cap: o.Cap, CapUSD: o.Cap.USD(),
			Statuses: map[string]int{}, Samples: map[string]string{},
			StartedAt: time.Now().UTC(),
		},
	}
	defer r.client.CloseIdleConnections()

	cfg, err := r.resetOracle(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.checkTestCanFail(cfg); err != nil {
		return nil, err
	}

	r.wave(ctx, 0, o.Requests, o.Concurrency)
	if o.Wave2 > 0 {
		if _, err := r.drain(ctx); err != nil {
			return nil, err
		}
		select {
		case <-time.After(o.Settle):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.wave(ctx, o.Requests, o.Wave2, min(o.Concurrency, o.Wave2))
	}
	snap, err := r.drain(ctx)
	if err != nil {
		return nil, err
	}

	rep := r.r
	rep.Provider = snap
	rep.Billed = snap.Billed
	rep.BilledUSD = snap.Billed.USD()
	if snap.Billed > o.Cap {
		rep.Overshoot = snap.Billed - o.Cap
		rep.OvershootPct = rep.Overshoot.USD() / o.Cap.USD() * 100
	}
	rep.UsablePct = snap.Billed.USD() / o.Cap.USD() * 100
	rep.DurationMS = time.Since(rep.StartedAt).Milliseconds()
	rep.Verdict, rep.Reasons, rep.Warnings = judge(rep, o.Scenario)
	return rep, nil
}

func endpoint(target, format string) string {
	base := strings.TrimSuffix(strings.TrimRight(target, "/"), "/v1")
	if format == "anthropic" {
		return base + "/v1/messages"
	}
	return base + "/v1/chat/completions"
}

func buildHeaders(o Options) http.Header {
	h := http.Header{"Content-Type": {"application/json"}}
	if o.APIKey != "" {
		if o.Format == "anthropic" {
			h.Set("x-api-key", o.APIKey)
		} else {
			h.Set("Authorization", "Bearer "+o.APIKey)
		}
	}
	if o.Format == "anthropic" {
		h.Set("anthropic-version", "2023-06-01")
	}
	for _, raw := range o.Headers {
		name, value, _ := strings.Cut(raw, ":")
		h.Set(strings.TrimSpace(name), strings.ReplaceAll(strings.TrimSpace(value), "{run}", o.RunID))
	}
	return h
}

func (r *run) resetOracle(ctx context.Context) (mock.Config, error) {
	overrides := r.o.Mock
	if len(overrides) == 0 {
		overrides = json.RawMessage("{}")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.o.Oracle, "/")+"/_oracle/reset", bytes.NewReader(overrides))
	if err != nil {
		return mock.Config{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return mock.Config{}, fmt.Errorf("oracle unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return mock.Config{}, fmt.Errorf("oracle reset: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return mock.Config{}, fmt.Errorf("oracle refused reset (status %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var snap mock.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return mock.Config{}, fmt.Errorf("oracle reset: %w", err)
	}
	if snap.Requests != 0 || snap.Billed != 0 {
		return mock.Config{}, errors.New("oracle did not reset to zero")
	}
	return snap.Config, nil
}

// checkTestCanFail refuses a run that could not catch an overspend, or whose
// cap would reject every call, because either result would prove nothing.
func (r *run) checkTestCanFail(cfg mock.Config) error {
	input := mock.InputTokens(r.messages(0))
	output := cfg.OutputTokens
	if !cfg.IgnoreMaxTokens {
		output = min(output, r.o.MaxTokens)
	}
	cost := money.Add(money.Mul(cfg.InputRate, int64(input)), money.Mul(cfg.OutputRate, int64(output)))
	if cost <= 0 {
		return errors.New("calls cost nothing at these rates, so no cap can be tested")
	}
	if cost > r.o.Cap {
		return fmt.Errorf("one call costs %s, more than the %s cap, so no call could be admitted", cost, r.o.Cap)
	}
	total := r.o.Requests + r.o.Wave2
	if most := money.Mul(cost, int64(total)); most <= r.o.Cap {
		return fmt.Errorf("%d calls cost at most %s, which cannot exceed the %s cap, so the test cannot fail", total, most, r.o.Cap)
	}
	r.r.CallCost = cost
	r.r.CallsCapAllows = int64(r.o.Cap / cost)
	return nil
}

// messages builds the message list for one call. Every prompt is unique, or a
// response cache would make the gateway look thrifty, and all have the same
// length, so every call costs the same.
func (r *run) messages(seq int) []byte {
	prefix := fmt.Sprintf("capproof %s %08d ", r.o.RunID, seq)
	fill := max(0, r.o.PromptBytes-len(prefix))
	prompt := prefix + strings.Repeat("word ", fill/5+1)[:fill]
	raw, _ := json.Marshal([]map[string]string{{"role": "user", "content": prompt}})
	return raw
}

func (r *run) body(seq int) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":      r.o.Model,
		"messages":   json.RawMessage(r.messages(seq)),
		"max_tokens": r.o.MaxTokens,
		"stream":     r.o.Stream,
	})
	return raw
}

func (r *run) wave(ctx context.Context, from, count, concurrency int) {
	start := make(chan struct{})
	slots := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			slots <- struct{}{}
			defer func() { <-slots }()
			r.call(ctx, from+i)
		}()
	}
	close(start)
	wg.Wait()
}

func (r *run) call(ctx context.Context, seq int) {
	ctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(r.body(seq)))
	if err != nil {
		r.record(func(rep *Report) { rep.Sent++; rep.TransportErrors++; rep.Samples["transport"] = err.Error() })
		return
	}
	req.Header = r.headers.Clone()

	resp, err := r.client.Do(req)
	if err != nil {
		r.record(func(rep *Report) { rep.Sent++; rep.TransportErrors++; rep.Samples["transport"] = err.Error() })
		return
	}
	defer resp.Body.Close()
	status := strconv.Itoa(resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		sample, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		r.record(func(rep *Report) {
			rep.Sent++
			rep.Statuses[status]++
			if _, seen := rep.Samples[status]; !seen {
				rep.Samples[status] = strings.TrimSpace(string(sample))
			}
		})
		return
	}

	var readErr error
	if r.o.Scenario == Disconnect {
		// Returning cancels the context, which drops the connection mid-stream.
		_, readErr = io.ReadFull(resp.Body, make([]byte, 1))
	} else {
		_, readErr = io.Copy(io.Discard, resp.Body)
	}
	r.record(func(rep *Report) {
		rep.Sent++
		rep.Admitted++
		rep.Statuses[status]++
		if readErr != nil {
			rep.BrokenStreams++
			rep.Samples["broken_stream"] = readErr.Error()
		}
	})
}

func (r *run) record(update func(*Report)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	update(r.r)
}

// drain waits until the provider has been idle on consecutive polls. A client
// finishing does not mean spending has: a dropped stream can keep billing.
func (r *run) drain(ctx context.Context) (mock.Snapshot, error) {
	deadline := time.Now().Add(r.o.Drain)
	url := strings.TrimRight(r.o.Oracle, "/") + "/_oracle"
	idle := 0
	for {
		snap, err := r.readOracle(ctx, url)
		if err != nil {
			return mock.Snapshot{}, err
		}
		if snap.InFlight == 0 {
			if idle++; idle == 3 {
				return snap, nil
			}
		} else {
			idle = 0
		}
		if time.Now().After(deadline) {
			return mock.Snapshot{}, fmt.Errorf("provider still had %d calls in flight after %s", snap.InFlight, r.o.Drain)
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return mock.Snapshot{}, ctx.Err()
		}
	}
}

func (r *run) readOracle(ctx context.Context, url string) (mock.Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return mock.Snapshot{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return mock.Snapshot{}, fmt.Errorf("reading oracle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mock.Snapshot{}, fmt.Errorf("reading oracle: status %d", resp.StatusCode)
	}
	var snap mock.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&snap); err != nil {
		return mock.Snapshot{}, fmt.Errorf("reading oracle: %w", err)
	}
	return snap, nil
}

// judge turns the numbers into a verdict. An overspend is decided first, because
// money past the cap is a failure however messy the rest of the run was. HELD
// must then be earned: something reached the provider, and the gateway refused
// the rest for what looks like a budget reason.
func judge(rep *Report, scenario string) (verdict string, reasons, warnings []string) {
	configFailures, serverFailures, refusals := classify(rep.Statuses)
	provider := rep.Provider

	if provider.Unrouted > 0 {
		warnings = append(warnings, fmt.Sprintf("the gateway called %d unknown provider route(s), last %q", provider.Unrouted, provider.LastUnrouted))
	}
	if provider.Rejected > 0 {
		warnings = append(warnings, fmt.Sprintf("the provider rejected %d malformed call(s)", provider.Rejected))
	}
	if rep.BrokenStreams > 0 {
		warnings = append(warnings, fmt.Sprintf("%d response(s) failed partway through", rep.BrokenStreams))
	}
	if provider.Requests < int64(rep.Admitted) {
		warnings = append(warnings, fmt.Sprintf("%d call(s) succeeded but only %d reached the provider; something answered without it", rep.Admitted, provider.Requests))
	}
	if provider.Requests > int64(rep.Admitted) {
		warnings = append(warnings, fmt.Sprintf("the provider saw %d call(s) for %d success(es); the gateway may be retrying", provider.Requests, rep.Admitted))
	}
	if scenario == Burst && rep.Sent >= 2 && provider.MaxInFlight < 2 {
		warnings = append(warnings, "calls never overlapped at the provider, so concurrency was not really tested")
	}
	if scenario == Disconnect && provider.Disconnects == 0 && rep.Admitted > 0 {
		warnings = append(warnings, "no disconnect reached the provider; the gateway may read streams to the end")
	}

	switch {
	case rep.Billed > rep.Cap:
		return Breached, []string{fmt.Sprintf("the provider billed %s against a %s cap", rep.Billed, rep.Cap)}, warnings
	case provider.Requests == 0:
		return Inconclusive, []string{"no call reached the provider, so the gateway's setup is the likely problem"}, warnings
	case configFailures > 0:
		return Inconclusive, []string{fmt.Sprintf("%d call(s) got an authentication, routing or validation status, which is not a budget decision", configFailures)}, warnings
	case serverFailures > 0 || rep.TransportErrors > 0:
		return Inconclusive, []string{fmt.Sprintf("%d server error(s) and %d transport error(s), so stopping early does not show a budget decision", serverFailures, rep.TransportErrors)}, warnings
	case refusals == 0:
		return Inconclusive, []string{"nothing was refused, yet spend stayed under the cap, so the cap was never exercised"}, warnings
	}
	if rep.UsablePct < 50 {
		warnings = append(warnings, fmt.Sprintf("only %.1f%% of the budget was usable; check the sample that these refusals were budget decisions", rep.UsablePct))
	}
	return Held, []string{fmt.Sprintf("the provider billed %s against a %s cap, and %d call(s) were refused", rep.Billed, rep.Cap, refusals)}, warnings
}

func classify(statuses map[string]int) (config, server, refusals int) {
	for raw, n := range statuses {
		code, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			server += n
		case code < 300:
		case code < 400, code == 401, code == 404, code == 405, code == 413, code == 415, code == 422:
			config += n
		case code == 408, code >= 500:
			server += n
		default:
			refusals += n
		}
	}
	return config, server, refusals
}

// Text renders the report for a terminal.
func (rep *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %s\n", "verdict", rep.Verdict)
	fmt.Fprintf(&b, "%-10s %s stream=%t via %s (run %s)\n", "scenario", rep.Scenario, rep.Stream, rep.Target, rep.RunID)
	fmt.Fprintf(&b, "%-10s %s\n", "cap", rep.Cap)
	fmt.Fprintf(&b, "%-10s %s (%.1f%% of the cap)\n", "billed", rep.Billed, rep.UsablePct)
	if rep.Overshoot > 0 {
		fmt.Fprintf(&b, "%-10s %s (+%.1f%%)\n", "overshoot", rep.Overshoot, rep.OvershootPct)
	}
	fmt.Fprintf(&b, "%-10s %d reached the provider; the cap allows %d at %s each\n", "calls", rep.Provider.Requests, rep.CallsCapAllows, rep.CallCost)
	fmt.Fprintf(&b, "%-10s %v, %d transport error(s)\n", "responses", rep.Statuses, rep.TransportErrors)
	for _, reason := range rep.Reasons {
		fmt.Fprintf(&b, "%-10s %s\n", "why", reason)
	}
	for _, warning := range rep.Warnings {
		fmt.Fprintf(&b, "%-10s %s\n", "warning", warning)
	}
	for status, sample := range rep.Samples {
		fmt.Fprintf(&b, "%-10s %s: %s\n", "sample", status, sample)
	}
	return b.String()
}
