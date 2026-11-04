package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/capd"
	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

// TestSurvivesKill kills the real binary with streams in flight, repeatedly. The
// provider keeps billing them, so the ledger must never read lower than the bill.
func TestSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and repeatedly kills a process")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "capd.exe")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	cfg := mock.DefaultConfig()
	cfg.LatencyMS, cfg.ChunkDelayMS, cfg.Chunks = 5, 40, 10
	provider, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(provider.Handler())
	defer upstream.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	const key, limit = "sk-crash-test", "0.05"
	cap, err := money.ParseUSD(limit)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "ledger.db")
	raw, _ := json.Marshal(capd.Config{
		Listen: addr, DB: db, Synchronous: "FULL", OpenAIURL: upstream.URL,
		Keys:   []capd.KeyConfig{{Name: "ci", SHA256: capd.HashKey(key), BudgetUSD: limit}},
		Prices: map[string]capd.ModelPrice{"gpt-4o-mini": {Input: "0.15", Output: "0.60", CacheWrite: "0.15"}},
	})
	config := filepath.Join(dir, "capd.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	body := `{"model":"gpt-4o-mini","max_tokens":500,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	for round := range 4 {
		proc := exec.Command(binary, "-config", config)
		if err := proc.Start(); err != nil {
			t.Fatal(err)
		}
		waitHealthy(t, client, addr)

		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+key)
				resp, err := client.Do(req)
				if err != nil {
					return // expected: the process is about to die
				}
				defer resp.Body.Close()
				_, _ = io.Copy(io.Discard, resp.Body)
			}()
		}
		time.Sleep(time.Duration(30+round*40) * time.Millisecond)
		if err := proc.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = proc.Wait()
		wg.Wait()
		waitIdle(t, provider)

		ledger, recovered, err := capd.Open(db, "FULL")
		if err != nil {
			t.Fatalf("round %d: the ledger did not reopen after the kill: %v", round, err)
		}
		b, err := ledger.Balance(context.Background(), "key:ci")
		ledger.Close()
		if err != nil {
			t.Fatal(err)
		}
		billed := provider.Snapshot().Billed
		t.Log(fmt.Sprintf("round %d: recovered %d holds, ledger %s, billed %s", round, recovered, b.Spent, billed))
		if b.Held != 0 {
			t.Errorf("round %d: I4: %s still held after recovery", round, b.Held)
		}
		if b.Spent < billed {
			t.Errorf("round %d: I3: ledger %s below the bill %s", round, b.Spent, billed)
		}
		if billed > cap || b.Spent > cap {
			t.Errorf("round %d: past the %s cap: ledger %s, billed %s", round, cap, b.Spent, billed)
		}
	}
	if provider.Snapshot().Disconnects == 0 {
		t.Error("no stream was cut off, so the kills tested nothing")
	}
}

func waitHealthy(t *testing.T, client *http.Client, addr string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("capd never became healthy")
}

func waitIdle(t *testing.T, provider *mock.Server) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for provider.Snapshot().InFlight > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the provider never went idle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
