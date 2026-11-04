// Command capproof tests whether an LLM spend cap holds when things go wrong.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NAMAN-VERMA-02/capproof/internal/mock"
	"github.com/NAMAN-VERMA-02/capproof/internal/money"
)

const usage = `capproof tests whether an LLM spend cap holds when things go wrong.

Commands:
  mock    run the fake provider and billing oracle

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
	log.Printf("mock provider and oracle listening on %s", *addr)
	return serve(*addr, server.Handler())
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
