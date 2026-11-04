package mock

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOracleRequiresTheToken(t *testing.T) {
	s, err := New(fastConfig())
	if err != nil {
		t.Fatal(err)
	}
	s.OracleToken = "secret"
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	post(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":`+messages+`}`)
	billed := s.Snapshot().Billed

	for _, token := range []string{"", "wrong", "secre", "secrett"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/_oracle/reset", nil)
		req.Header.Set(TokenHeader, token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d", token, resp.StatusCode)
		}
	}
	if s.Snapshot().Billed != billed {
		t.Fatal("an unauthenticated caller wiped the ledger")
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/_oracle/reset", nil)
	req.Header.Set(TokenHeader, "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || s.Snapshot().Billed != 0 {
		t.Fatalf("the right token was refused: status %d", resp.StatusCode)
	}
}

func TestCallsWithoutTheRunIDAreFlagged(t *testing.T) {
	cfg := fastConfig()
	cfg.RunID = "run-1"
	s, ts := start(t, cfg)
	post(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"run-1 hi"}]}`)
	if got := s.Snapshot().Foreign; got != 0 {
		t.Fatalf("own call flagged: %d", got)
	}
	post(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":`+messages+`}`)
	got := s.Snapshot()
	if got.Foreign != 1 || got.Requests != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestOverloadIsRefusedUnbilled(t *testing.T) {
	s, ts := start(t, fastConfig())
	s.maxInFlight = 0
	for _, tt := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"m","messages":` + messages + `}`},
		{"/v1/messages", `{"model":"m","max_tokens":5,"messages":` + messages + `}`},
	} {
		if status, _ := post(t, ts.URL+tt.path, tt.body); status != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d", tt.path, status)
		}
	}
	got := s.Snapshot()
	if got.Billed != 0 || got.Requests != 0 || got.Overloaded != 2 || got.InFlight != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestLongUnroutedPathIsTruncated(t *testing.T) {
	s, ts := start(t, fastConfig())
	post(t, ts.URL+"/"+strings.Repeat("a", 5000), `{}`)
	if got := len(s.Snapshot().LastUnrouted); got > 200 {
		t.Fatalf("recorded %d bytes", got)
	}
}
