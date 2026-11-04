// Package lab measures real provider behaviour with a real API key and real money,
// so it is built to spend as little as possible: it refuses to start unless the worst
// case fits a ceiling, sends one call at a time, never retries, and stops at the first
// refusal.
package lab

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
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/meter"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

// Phases.
const (
	Full   = "full"   // read every stream to the end; the baseline
	Cancel = "cancel" // drop every stream after the first data line
	Cap    = "cap"    // keep calling until the provider refuses
)

// HardCeiling is the most one run may ever risk, whatever the flags say.
const HardCeiling = money.Pico(50_000_000_000_000)

const (
	callTimeout = 2 * time.Minute
	prompt      = "Count upward from 1, writing each number in words on its own line. Do not stop until you are told to."
)

// Options describes one run.
type Options struct {
	Provider   string // "openai" or "anthropic"
	BaseURL    string // empty means the provider's official API
	Model      string
	APIKey     string
	InputRate  money.Pico
	OutputRate money.Pico
	Phase      string
	Calls      int
	MaxTokens  int
	Delay      time.Duration
	MaxSpend   money.Pico
	Confirm    bool // without it, nothing is sent
}

// Report is the result of one run. Billing is read from the provider's dashboard
// afterwards, using the time window here: only the provider knows what it charged.
type Report struct {
	Phase         string     `json:"phase"`
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	DryRun        bool       `json:"dry_run"`
	WorstCase     money.Pico `json:"worst_case_pico"`
	WorstCaseUSD  float64    `json:"worst_case_usd"`
	Sent          int        `json:"sent"`
	Finished      int        `json:"finished"`
	Cancelled     int        `json:"cancelled"`
	Reported      money.Pico `json:"reported_cost_pico"`
	ReportedUSD   float64    `json:"reported_cost_usd"`
	UnknownCost   int        `json:"calls_with_unknown_cost"`
	RefusedStatus int        `json:"refused_status,omitempty"`
	RefusedBody   string     `json:"refused_body,omitempty"`
	FailedError   string     `json:"failed_error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       time.Time  `json:"ended_at"`
	SecondsToStop float64    `json:"seconds_to_stop,omitempty"`
}

func (o *Options) setDefaults() {
	if o.BaseURL == "" {
		switch o.Provider {
		case "openai":
			o.BaseURL = "https://api.openai.com"
		case "anthropic":
			o.BaseURL = "https://api.anthropic.com"
		}
	}
	if o.Calls == 0 {
		o.Calls = 20
	}
	if o.MaxTokens == 0 {
		o.MaxTokens = 300
	}
	if o.MaxSpend == 0 {
		o.MaxSpend = 1_000_000_000_000 // one dollar
	}
}

// checkHost keeps a real key from going anywhere unexpected: only the official APIs,
// or plain loopback for testing against the mock provider.
func checkHost(raw, provider string) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return errors.New("the base URL is not valid")
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return nil
	}
	official := map[string]string{"openai": "api.openai.com", "anthropic": "api.anthropic.com"}[provider]
	if u.Scheme != "https" || host != official {
		return fmt.Errorf("a real key may only go to https://%s", official)
	}
	return nil
}

// WorstCase is the most a run can cost: every call at its full limit. Input is
// bounded at one token per byte of the request.
func (o Options) WorstCase() money.Pico {
	inputBound := int64(len(prompt) + len(o.Model) + 200)
	perCall := money.Add(money.Mul(o.InputRate, inputBound), money.Mul(o.OutputRate, int64(o.MaxTokens)))
	return money.Mul(perCall, int64(o.Calls))
}

func (o Options) validate() error {
	if o.Provider != "openai" && o.Provider != "anthropic" {
		return fmt.Errorf("unknown provider %q; this lab tests openai and anthropic", o.Provider)
	}
	if o.Phase != Full && o.Phase != Cancel && o.Phase != Cap {
		return fmt.Errorf("unknown phase %q", o.Phase)
	}
	if err := checkHost(o.BaseURL, o.Provider); err != nil {
		return err
	}
	switch {
	case o.Model == "":
		return errors.New("name the model")
	case o.InputRate <= 0 || o.OutputRate <= 0:
		return errors.New("set both prices from the provider's price list; the spend check depends on them")
	case o.Calls < 1 || o.Calls > 1000:
		return errors.New("calls must be between 1 and 1000")
	case o.MaxTokens < 16 || o.MaxTokens > 4096:
		return errors.New("max tokens must be between 16 and 4096")
	case o.Delay < 0:
		return errors.New("the delay must not be negative")
	case o.MaxSpend <= 0 || o.MaxSpend > HardCeiling:
		return fmt.Errorf("the spend limit must be above zero and at most %s", HardCeiling)
	case o.WorstCase() > o.MaxSpend:
		return fmt.Errorf("the worst case is %s, above the %s limit; use fewer calls or fewer tokens", o.WorstCase(), o.MaxSpend)
	case o.Confirm && o.APIKey == "":
		return errors.New("set CAPPROOF_LAB_KEY; keys are never taken as flags")
	}
	return nil
}

func (o Options) request(ctx context.Context) (*http.Request, error) {
	fields := map[string]any{
		"model":    o.Model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	path := "/v1/messages"
	if o.Provider == "openai" {
		path = "/v1/chat/completions"
		fields["max_completion_tokens"] = o.MaxTokens
		fields["stream_options"] = map[string]bool{"include_usage": true}
	} else {
		fields["max_tokens"] = o.MaxTokens
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.Provider == "openai" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	} else {
		req.Header.Set("x-api-key", o.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	return req, nil
}

// Run executes the plan. Without Confirm it only reports the worst case.
func Run(ctx context.Context, o Options) (*Report, error) {
	o.setDefaults()
	if err := o.validate(); err != nil {
		return nil, err
	}
	rep := &Report{
		Phase: o.Phase, Provider: o.Provider, Model: o.Model, DryRun: !o.Confirm,
		WorstCase: o.WorstCase(), WorstCaseUSD: o.WorstCase().USD(), StartedAt: time.Now().UTC(),
	}
	defer func() {
		rep.EndedAt = time.Now().UTC()
		rep.ReportedUSD = rep.Reported.USD()
	}()
	if !o.Confirm {
		return rep, nil
	}
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	rates := meter.Rates{Input: o.InputRate, Output: o.OutputRate, CacheWrite: o.InputRate * 2, CacheRead: o.InputRate}
	for i := range o.Calls {
		if i > 0 {
			select {
			case <-time.After(o.Delay):
			case <-ctx.Done():
				return rep, nil
			}
		}
		if stop := oneCall(ctx, client, o, rates, rep); stop {
			rep.SecondsToStop = time.Since(rep.StartedAt).Seconds()
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return rep, nil
}

// oneCall sends one call and reports whether the run must stop. Any refusal or
// failure stops it: retrying costs money and could hide what is being measured.
func oneCall(ctx context.Context, client *http.Client, o Options, rates meter.Rates, rep *Report) (stop bool) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := o.request(ctx)
	if err != nil {
		rep.FailedError = err.Error()
		return true
	}
	rep.Sent++
	resp, err := client.Do(req)
	if err != nil {
		rep.FailedError = scrub(err.Error(), o.APIKey)
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		sample, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		rep.RefusedStatus = resp.StatusCode
		rep.RefusedBody = scrub(string(sample), o.APIKey)
		return true
	}

	tracker := meter.Tracker{Rates: rates}
	reader := bufio.NewReader(io.LimitReader(resp.Body, 16<<20))
	for {
		line, readErr := reader.ReadBytes('\n')
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if o.Phase == Cancel {
				rep.Cancelled++
				return false // returning cancels the context, which drops the connection
			}
			tracker.Feed(data)
		}
		if readErr != nil {
			break
		}
	}
	cost, known := tracker.Cost()
	if !tracker.Finished() || !known {
		rep.UnknownCost++
		return false
	}
	rep.Finished++
	rep.Reported = money.Add(rep.Reported, cost)
	return false
}

// scrub removes the key and makes provider text safe to print.
func scrub(text, key string) string {
	if key != "" {
		text = strings.ReplaceAll(text, key, "[redacted]")
	}
	quoted := fmt.Sprintf("%q", text)
	return quoted[1 : len(quoted)-1]
}

// Text renders the report for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	if r.DryRun {
		fmt.Fprintf(&b, "DRY RUN: nothing was sent. Add -confirm to send real calls.\n")
	}
	fmt.Fprintf(&b, "phase        %s on %s, model %s\n", r.Phase, r.Provider, r.Model)
	fmt.Fprintf(&b, "worst case   %s\n", r.WorstCase)
	if r.DryRun {
		return b.String()
	}
	fmt.Fprintf(&b, "calls        %d sent, %d finished, %d cancelled, %d with unknown cost\n", r.Sent, r.Finished, r.Cancelled, r.UnknownCost)
	fmt.Fprintf(&b, "usage cost   %s, from usage the provider reported on finished calls\n", r.Reported)
	if r.RefusedStatus != 0 {
		fmt.Fprintf(&b, "refused      status %d after %.1fs: %s\n", r.RefusedStatus, r.SecondsToStop, r.RefusedBody)
	}
	if r.FailedError != "" {
		fmt.Fprintf(&b, "failed       %s\n", r.FailedError)
	}
	fmt.Fprintf(&b, "window       %s to %s UTC\n", r.StartedAt.Format(time.DateTime), r.EndedAt.Format(time.DateTime))
	fmt.Fprintf(&b, "next         wait for the dashboard to update, then read what this key was billed in that window\n")
	return b.String()
}
