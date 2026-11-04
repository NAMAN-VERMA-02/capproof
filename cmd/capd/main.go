// Command capd is an LLM gateway that holds the worst-case cost of each call before
// forwarding it, so a budget survives bursts, disconnects and crashes.
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

	"github.com/NAMAN-VERMA-02/capproof/internal/capd"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-key" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: capd hash-key <virtual key>")
			os.Exit(2)
		}
		fmt.Println(capd.HashKey(os.Args[2]))
		return
	}
	path := flag.String("config", "capd.json", "config file")
	flag.Parse()
	if err := run(*path); err != nil {
		fmt.Fprintln(os.Stderr, "capd:", err)
		os.Exit(1)
	}
}

func run(path string) error {
	cfg, err := capd.LoadConfig(path)
	if err != nil {
		return err
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8787" // loopback unless deliberately exposed
	}
	if cfg.DB == "" {
		cfg.DB = "capd.db"
	}
	if cfg.Synchronous == "" {
		cfg.Synchronous = "FULL"
	}
	ledger, recovered, err := capd.Open(cfg.DB, cfg.Synchronous)
	if err != nil {
		return fmt.Errorf("opening the ledger: %w", err)
	}
	defer ledger.Close()
	if recovered > 0 {
		log.Printf("charged %d call(s) left open by a crash at their full hold", recovered)
	}
	server, err := capd.New(cfg, ledger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: cfg.Listen, Handler: server, ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	log.Printf("capd listening on %s", cfg.Listen)
	select {
	case err := <-failed:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
