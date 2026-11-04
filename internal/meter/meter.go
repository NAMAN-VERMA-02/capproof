// Package meter reads the parts of an LLM request and response that decide what a
// call costs. Requests are read defensively, because a gateway and a provider that
// disagree about a field can be played against each other.
package meter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

// Request is what a gateway must know about a call before forwarding it.
type Request struct {
	Model     string
	MaxOutput int64
	Stream    bool
	// HasUsageOption reports that the client set stream_options itself.
	HasUsageOption bool
}

// guarded fields decide the price, so ambiguity in any of them is refused.
var guarded = map[string]bool{
	"model": true, "max_tokens": true, "max_completion_tokens": true, "n": true,
	"stream": true, "stream_options": true, "service_tier": true, "speed": true, "inference_geo": true,
}

// Inspect walks the top level of a request body. It refuses duplicate or wrongly
// cased guarded fields, more than one choice, and settings known to raise the price,
// since each is a way to hold for one amount and be billed another.
func Inspect(body []byte) (Request, error) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return req, errors.New("the request must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return req, errors.New("the request is not valid JSON")
		}
		key := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return req, errors.New("the request is not valid JSON")
		}
		lower := strings.ToLower(key)
		if !guarded[lower] {
			continue
		}
		if lower != key || seen[lower] {
			return req, fmt.Errorf("the %q field is repeated or wrongly cased, so the cost is ambiguous", key)
		}
		seen[lower] = true
		if err := req.apply(lower, value); err != nil {
			return req, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return req, errors.New("the request is not valid JSON")
	}
	if dec.More() {
		return req, errors.New("the request has trailing data")
	}
	if req.MaxOutput == 0 {
		// Without an output limit the worst case is unbounded, so no hold can be sound.
		return req, errors.New("a positive max_tokens is required to bound the cost")
	}
	return req, nil
}

func (r *Request) apply(field string, value json.RawMessage) error {
	switch field {
	case "model":
		if err := json.Unmarshal(value, &r.Model); err != nil {
			return errors.New("model must be a string")
		}
	case "max_tokens", "max_completion_tokens":
		var n int64
		if err := json.Unmarshal(value, &n); err != nil || n < 1 {
			return fmt.Errorf("%s must be a positive integer", field)
		}
		r.MaxOutput = max(r.MaxOutput, n)
	case "n":
		var n int64
		if err := json.Unmarshal(value, &n); err != nil || n != 1 {
			return errors.New("only n=1 can be held for")
		}
	case "stream":
		if err := json.Unmarshal(value, &r.Stream); err != nil {
			return errors.New("stream must be true or false")
		}
	case "stream_options":
		r.HasUsageOption = true
	case "service_tier":
		var tier string
		_ = json.Unmarshal(value, &tier)
		if tier != "auto" && tier != "default" && tier != "standard_only" {
			return fmt.Errorf("service_tier %q may raise the price, so it is refused", tier)
		}
	default: // speed, inference_geo
		return fmt.Errorf("%s may raise the price, so it is refused", field)
	}
	return nil
}

// Rates are the prices of one model, per token.
type Rates struct {
	Input, Output, CacheWrite, CacheRead money.Pico
}

// Tracker keeps the latest usage seen in a response. Streams report usage
// cumulatively, so the last value wins, and malformed usage makes the cost unknown.
type Tracker struct {
	Rates                                Rates
	input, output, cacheWrite, cacheRead *int64
	malformed, finished                  bool
}

// Feed takes one JSON document, or the payload of one SSE data line.
func (t *Tracker) Feed(data []byte) {
	data = bytes.TrimSpace(data)
	if string(data) == "[DONE]" {
		t.finished = true
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
		t.finished = true
	}
	for _, raw := range []json.RawMessage{doc.Usage, doc.Message.Usage} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var f struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			InputTokens      *int64 `json:"input_tokens"`
			OutputTokens     *int64 `json:"output_tokens"`
			CacheWrite       *int64 `json:"cache_creation_input_tokens"`
			CacheRead        *int64 `json:"cache_read_input_tokens"`
		}
		if json.Unmarshal(raw, &f) != nil {
			t.malformed = true
			continue
		}
		assign := func(dst **int64, values ...*int64) {
			for _, v := range values {
				if v != nil {
					*dst = v
				}
			}
		}
		assign(&t.input, f.PromptTokens, f.InputTokens)
		assign(&t.output, f.CompletionTokens, f.OutputTokens)
		assign(&t.cacheWrite, f.CacheWrite)
		assign(&t.cacheRead, f.CacheRead)
	}
}

// Finished reports that a stream reached its terminator. A stream cut off cleanly
// also ends in EOF, with usage that stops early, so EOF alone proves nothing.
func (t *Tracker) Finished() bool { return t.finished }

// Cost returns the cost of the usage seen, and whether it is known.
func (t *Tracker) Cost() (money.Pico, bool) {
	if t.malformed || t.input == nil || t.output == nil {
		return 0, false
	}
	total := money.Pico(0)
	for _, part := range []struct {
		tokens *int64
		rate   money.Pico
	}{{t.input, t.Rates.Input}, {t.output, t.Rates.Output}, {t.cacheWrite, t.Rates.CacheWrite}, {t.cacheRead, t.Rates.CacheRead}} {
		if part.tokens == nil {
			continue
		}
		if *part.tokens < 0 {
			return 0, false
		}
		total = money.Add(total, money.Mul(part.rate, *part.tokens))
	}
	return total, true
}
