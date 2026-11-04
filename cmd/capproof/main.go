// Command capproof tests whether an LLM spend cap holds when things go wrong.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/control"
	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
	"github.com/NAMAN-VERMA-02/capproof/internal/runner"
)

const usage = `capproof tests whether an LLM spend cap holds when things go wrong.

Commands:
  mock    run the fake provider and billing oracle
  control run a calibration gateway: naive must fail, strict must pass
  run     send a scenario through a gateway and judge it against the oracle

Exit codes for run: 0 held, 1 breached, 2 inconclusive, 3 could not run.

Run "capproof <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(3)
	}
	var err error
	switch os.Args[1] {
	case "mock":
		err = runMock(os.Args[2:])
	case "control":
		err = runControl(os.Args[2:])
	case "run":
		err = runScenario(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(3)
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "capproof:", err)
		os.Exit(3)
	}
}

func runMock(args []string) error {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	cfg := mock.DefaultConfig()
	addr := fs.String("addr", "127.0.0.1:9400", "listen address; loopback by default because the oracle has no auth")
	inRate := fs.String("input-usd-per-mtok", "0.15", "input price in dollars per million tokens")
	outRate := fs.String("output-usd-per-mtok", "0.60", "output price in dollars per million tokens")
	fs.IntVar(&cfg.OutputTokens, "output-tokens", cfg.OutputTokens, "tokens generated per call, before the request's limit")
	fs.IntVar(&cfg.Chunks, "chunks", cfg.Chunks, "stream chunks per call")
	fs.IntVar(&cfg.LatencyMS, "latency-ms", cfg.LatencyMS, "delay before the first token")
	fs.IntVar(&cfg.ChunkDelayMS, "chunk-delay-ms", cfg.ChunkDelayMS, "delay between stream chunks")
	fs.StringVar(&cfg.OnDisconnect, "on-disconnect", cfg.OnDisconnect, `"full" keeps billing after the client leaves, "delivered" stops`)
	fs.StringVar(&cfg.Usage, "usage", cfg.Usage, `reported usage: "normal", "omit" or "malformed"`)
	fs.BoolVar(&cfg.IgnoreMaxTokens, "ignore-max-tokens", cfg.IgnoreMaxTokens, "generate past the request's token limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var err error
	if cfg.InputRate, err = money.ParseRate(*inRate); err != nil {
		return fmt.Errorf("-input-usd-per-mtok: %w", err)
	}
	if cfg.OutputRate, err = money.ParseRate(*outRate); err != nil {
		return fmt.Errorf("-output-usd-per-mtok: %w", err)
	}
	server, err := mock.New(cfg)
	if err != nil {
		return err
	}
	server.OracleToken = os.Getenv("CAPPROOF_ORACLE_TOKEN")
	if server.OracleToken == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		server.OracleToken = hex.EncodeToString(b[:])
		log.Printf("set CAPPROOF_ORACLE_TOKEN=%s for capproof run", server.OracleToken)
	}
	log.Printf("mock provider and oracle listening on %s", *addr)
	return serve(*addr, server.Handler())
}

func runControl(args []string) error {
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	mode := fs.String("mode", control.Strict, "naive or strict")
	addr := fs.String("addr", "127.0.0.1:9401", "listen address")
	upstream := fs.String("upstream", "http://127.0.0.1:9400", "base URL of the provider")
	capUSD := fs.String("cap-usd", "", "budget in dollars")
	inRate := fs.String("input-usd-per-mtok", "0.15", "input price in dollars per million tokens")
	outRate := fs.String("output-usd-per-mtok", "0.60", "output price in dollars per million tokens")
	if err := fs.Parse(args); err != nil {
		return err
	}
	limit, err := money.ParseUSD(*capUSD)
	if err != nil {
		return fmt.Errorf("-cap-usd: %w", err)
	}
	in, err := money.ParseRate(*inRate)
	if err != nil {
		return fmt.Errorf("-input-usd-per-mtok: %w", err)
	}
	out, err := money.ParseRate(*outRate)
	if err != nil {
		return fmt.Errorf("-output-usd-per-mtok: %w", err)
	}
	gw, err := control.New(*mode, *upstream, limit, in, out)
	if err != nil {
		return err
	}
	log.Printf("%s control gateway listening on %s", *mode, *addr)
	return serve(*addr, gw)
}

type headerList []string

func (h *headerList) String() string     { return strings.Join(*h, ", ") }
func (h *headerList) Set(v string) error { *h = append(*h, v); return nil }

func runScenario(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o runner.Options
	var headers headerList
	fs.StringVar(&o.Scenario, "scenario", runner.Burst, "burst, sequential or disconnect")
	fs.StringVar(&o.Target, "target", "", "base URL of the gateway under test")
	fs.StringVar(&o.Oracle, "oracle", "http://127.0.0.1:9400", "base URL of the mock provider")
	fs.StringVar(&o.Format, "format", "openai", "openai or anthropic")
	fs.StringVar(&o.Model, "model", "gpt-4o-mini", "model name, as the gateway expects it")
	capUSD := fs.String("cap-usd", "", "budget configured on the gateway, in dollars")
	fs.Var(&headers, "H", `extra header, repeatable; {run} expands to the run ID, for example -H "X-Run-Id: {run}"`)
	fs.IntVar(&o.Requests, "requests", 30, "calls in the first wave")
	fs.IntVar(&o.Concurrency, "concurrency", 0, "calls at once; defaults to all of them for burst and 1 otherwise")
	fs.IntVar(&o.Wave2, "wave2", 0, "calls sent after the settle wait, to catch late accounting")
	fs.DurationVar(&o.Settle, "settle", 15*time.Second, "wait before the second wave")
	fs.IntVar(&o.MaxTokens, "max-tokens", 500, "max_tokens on each call")
	fs.IntVar(&o.PromptBytes, "prompt-bytes", 4000, "prompt size on each call")
	fs.BoolVar(&o.Stream, "stream", false, "stream responses; always on for the disconnect scenario")
	fs.DurationVar(&o.Timeout, "timeout", 60*time.Second, "timeout per call")
	fs.DurationVar(&o.Drain, "drain", 60*time.Second, "how long the provider may take to go idle")
	fs.StringVar(&o.RunID, "run-id", "", "run ID; generated when empty")
	mockJSON := fs.String("mock", "", `provider overrides for this run, for example '{"usage":"omit"}'`)
	jsonOut := fs.String("json", "", "also write the full report to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var err error
	if o.Cap, err = money.ParseUSD(*capUSD); err != nil {
		return fmt.Errorf("-cap-usd: %w", err)
	}
	o.Headers = headers
	o.Mock = json.RawMessage(*mockJSON)
	// From the environment, so the key stays out of the process list and shell history.
	o.APIKey = os.Getenv("CAPPROOF_API_KEY")
	o.OracleToken = os.Getenv("CAPPROOF_ORACLE_TOKEN")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := runner.Run(ctx, o)
	if err != nil {
		return err
	}
	fmt.Print(rep.Text())
	if *jsonOut != "" {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonOut, append(raw, 0x0a), 0o644); err != nil {
			return err
		}
	}
	switch rep.Verdict {
	case runner.Held:
		return nil
	case runner.Breached:
		os.Exit(1)
	}
	os.Exit(2)
	return nil
}

// serve runs until interrupted, then lets in-flight calls finish so their bills
// are complete. It sets no write timeout because streams are long-lived.
func serve(addr string, handler http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
