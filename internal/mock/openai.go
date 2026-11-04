package mock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      json.RawMessage `json:"messages"`
	Tools         json.RawMessage `json:"tools"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens           *int `json:"max_tokens"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	N                   *int `json:"n"`
}

func openAIError(msg string) any {
	return map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": nil}}
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r, openAIError)
	if !ok {
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.reject(w, http.StatusBadRequest, openAIError("invalid JSON: "+err.Error()))
		return
	}
	limit := 0
	switch {
	case req.Model == "":
		s.reject(w, http.StatusBadRequest, openAIError("model is required"))
		return
	case !isNonEmptyArray(req.Messages):
		s.reject(w, http.StatusBadRequest, openAIError("messages must be a non-empty array"))
		return
	case req.N != nil && *req.N != 1:
		// shortcut: n>1 is refused rather than billed per choice, add it when a gateway test needs it.
		s.reject(w, http.StatusBadRequest, openAIError("the mock supports only n=1"))
		return
	}
	// max_completion_tokens is the current name; max_tokens is the older alias.
	for _, candidate := range []*int{req.MaxTokens, req.MaxCompletionTokens} {
		if candidate == nil {
			continue
		}
		if *candidate < 1 {
			s.reject(w, http.StatusBadRequest, openAIError("token limit must be at least 1"))
			return
		}
		limit = *candidate
	}

	cfg := s.config()
	input := InputTokens(req.Messages, req.Tools)
	output, truncated := outputTokens(cfg, limit)
	s.begin(cfg, input)
	defer s.end()

	finish := "stop"
	if truncated {
		finish = "length"
	}
	id := fmt.Sprintf("chatcmpl-mock-%d", s.seq.Add(1))
	created := time.Now().Unix()

	if !req.Stream {
		connected := s.generate(r.Context(), cfg, output, 1, func(int) error { return nil })
		if !connected {
			return
		}
		resp := map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text(output)},
				"finish_reason": finish,
			}},
		}
		if usage := openAIUsage(cfg, input, output); usage != nil {
			resp["usage"] = usage
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	rc := http.NewResponseController(w)
	setStreamHeaders(w)
	chunk := func(delta, finishReason any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
		}
	}
	first := true
	connected := s.generate(r.Context(), cfg, output, cfg.Chunks, func(tokens int) error {
		delta := map[string]any{"content": text(tokens)}
		if first {
			delta["role"] = "assistant"
			first = false
		}
		return sse(w, rc, "", chunk(delta, nil))
	})
	if !connected {
		return
	}
	if err := sse(w, rc, "", chunk(map[string]any{}, finish)); err != nil {
		s.noteDisconnect()
		return
	}
	// Like the real API, usage appears only when the client asks for it, which
	// is why a gateway that forgets to request it has nothing to settle with.
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		final := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{},
		}
		if usage := openAIUsage(cfg, input, output); usage != nil {
			final["usage"] = usage
		}
		if err := sse(w, rc, "", final); err != nil {
			s.noteDisconnect()
			return
		}
	}
	if err := sse(w, rc, "", "[DONE]"); err != nil {
		s.noteDisconnect()
	}
}

func openAIUsage(cfg Config, input, output int) any {
	switch cfg.Usage {
	case "omit":
		return nil
	case "malformed":
		return map[string]any{"prompt_tokens": "oops", "completion_tokens": nil}
	}
	return map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
}
