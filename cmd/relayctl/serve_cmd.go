package main

import (
	"context"
	"fmt"
	"io"
	"os"
	ossignal "os/signal"
	"syscall"

	"pairwise.local/relayctl/internal/guard"
	"pairwise.local/relayctl/internal/impair"
	"pairwise.local/relayctl/internal/relay"
	"pairwise.local/relayctl/internal/signal"
)

func runServe(args []string, stderr io.Writer) int {
	flags := newFlagSet("serve", stderr)
	listen := flags.String("listen", "127.0.0.1:47000", "loopback signaling address")
	impairSpec := flags.String("impair", "", "delay=20ms,reorder=5,loss=0.01,duplicate=0,seed=7")
	if !parseFlags(flags, args) {
		return exitConfig
	}
	if err := guard.ValidateListenAddress(*listen); err != nil {
		fmt.Fprintf(stderr, "refusing listener: %v\n", err)
		return codeForError(err)
	}
	impairConfig, err := impair.ParseSpec(*impairSpec)
	if err != nil {
		fmt.Fprintf(stderr, "invalid impairment: %v\n", err)
		return exitConfig
	}
	engineConfig := relay.DefaultConfig()
	engineConfig.Impair = impairConfig
	engine, err := relay.NewEngine(engineConfig)
	if err != nil {
		fmt.Fprintf(stderr, "create relay engine: %v\n", err)
		return exitConfig
	}
	defer engine.Close()
	server := signal.NewServer(engine)
	if err := server.Listen(*listen); err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		return codeForError(err)
	}
	defer server.Close()
	ctx, cancel := ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Fprintf(stderr, "relayctl listening on %s\n", server.Addr())
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx) }()
	select {
	case <-ctx.Done():
		return exitOK
	case err := <-serveErr:
		if err == nil || ctx.Err() != nil {
			return exitOK
		}
		fmt.Fprintf(stderr, "signal server: %v\n", err)
		return codeForError(err)
	}
}
