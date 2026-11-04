package mock

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type messagesRequest struct {
	Model     string          `json:"model"`
	Messages  json.RawMessage `json:"messages"`
	System    json.RawMessage `json:"system"`
	Tools     json.RawMessage `json:"tools"`
	Stream    bool            `json:"stream"`
	MaxTokens *int            `json:"max_tokens"`
}

func anthropicError(msg string) any {
	return map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": msg}}
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r, anthropicError)
	if !ok {
		return
	}
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.reject(w, http.StatusBadRequest, anthropicError("invalid JSON: "+err.Error()))
		return
	}
	switch {
	case req.Model == "":
		s.reject(w, http.StatusBadRequest, anthropicError("model is required"))
		return
	case !isNonEmptyArray(req.Messages):
		s.reject(w, http.StatusBadRequest, anthropicError("messages must be a non-empty array"))
		return
	case req.MaxTokens == nil || *req.MaxTokens < 1: // required by the real API
		s.reject(w, http.StatusBadRequest, anthropicError("max_tokens must be at least 1"))
		return
	}

	cfg := s.config()
	input := InputTokens(req.Messages, req.System, req.Tools)
	output, truncated := outputTokens(cfg, *req.MaxTokens)
	if !s.begin(cfg, input, body) {
		writeJSON(w, http.StatusServiceUnavailable, anthropicError("the mock provider is overloaded"))
		return
	}
	defer s.end()

	stop := "end_turn"
	if truncated {
		stop = "max_tokens"
	}
	id := fmt.Sprintf("msg_mock_%d", s.seq.Add(1))

	if !req.Stream {
		connected := s.generate(r.Context(), cfg, output, 1, func(int) error { return nil })
		if !connected {
			return
		}
		resp := map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content":     []any{map[string]any{"type": "text", "text": text(output)}},
			"stop_reason": stop, "stop_sequence": nil,
		}
		if usage := anthropicUsage(cfg, input, output); usage != nil {
			resp["usage"] = usage
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	rc := http.NewResponseController(w)
	setStreamHeaders(w)
	first := true
	connected := s.generate(r.Context(), cfg, output, cfg.Chunks, func(tokens int) error {
		if first {
			first = false
			message := map[string]any{
				"id": id, "type": "message", "role": "assistant", "model": req.Model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			}
			if usage := anthropicUsage(cfg, input, 1); usage != nil {
				message["usage"] = usage
			}
			if err := sse(w, rc, "message_start", map[string]any{"type": "message_start", "message": message}); err != nil {
				return err
			}
			if err := sse(w, rc, "content_block_start", map[string]any{
				"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
		}
		return sse(w, rc, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text(tokens)},
		})
	})
	if !connected {
		return
	}
	delta := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
	}
	// The real API reports cumulative output here, so a parser must keep the last value.
	if usage := anthropicUsage(cfg, input, output); usage != nil {
		if cfg.Usage == "malformed" {
			delta["usage"] = usage
		} else {
			delta["usage"] = map[string]any{"output_tokens": output}
		}
	}
	for _, event := range []struct {
		name    string
		payload any
	}{
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"message_delta", delta},
		{"message_stop", map[string]any{"type": "message_stop"}},
	} {
		if err := sse(w, rc, event.name, event.payload); err != nil {
			s.noteDisconnect()
			return
		}
	}
}

func anthropicUsage(cfg Config, input, output int) any {
	switch cfg.Usage {
	case "omit":
		return nil
	case "malformed":
		return map[string]any{"input_tokens": "oops", "output_tokens": nil}
	}
	return map[string]any{"input_tokens": input, "output_tokens": output}
}
