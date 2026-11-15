package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	ossignal "os/signal"
	"syscall"
	"time"

	"relayd/internal/signal"
)

func cmdSignal(args []string) int {
	fs := flag.NewFlagSet("signal", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "loopback listen address")
	expectReject := fs.Int("expect-reject", -1, "if >= 0, run a self-check expecting this many rejected messages")
	if err := fs.Parse(args); err != nil {
		return exitSignal
	}
	srv, err := signal.Listen(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	ctx, stop := osSignalContext()
	defer stop()
	go srv.Serve(ctx)

	if *expectReject >= 0 {
		return runRejectProbe(srv, *expectReject)
	}
	fmt.Fprintf(os.Stderr, "signal: listening on %s\n", srv.Addr())
	<-ctx.Done()
	return exitOK
}

// runRejectProbe dials two clients, sends one duplicate offer (stale seq)
// and verifies the server rejects exactly expect messages.
func runRejectProbe(srv *signal.Server, expect int) int {
	addr := srv.Addr()
	a, err := signal.Dial(addr, "probe", "a", "probe")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	defer a.Close()
	b, err := signal.Dial(addr, "probe", "b", "probe")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// First offer (seq 1) is accepted and routed to b.
	if err := a.SendSeq(1, signal.Message{Kind: signal.KindOffer, To: "b", SDP: "v=0"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	if _, err := b.Recv(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "probe: no delivery:", err)
		return exitSignal
	}
	// Replay the exact same message: stale seq, must be rejected.
	if err := a.SendSeq(1, signal.Message{Kind: signal.KindOffer, To: "b", SDP: "v=0"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitSignal
	}
	// The server notifies the sender about rejections; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for srv.Rejected() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rejected := int(srv.Rejected())
	fmt.Printf("{\"rejected\":%d}\n", rejected)
	if rejected != expect {
		fmt.Fprintf(os.Stderr, "signal: rejected=%d, want %d\n", rejected, expect)
		return exitSignal
	}
	return exitOK
}

func osSignalContext() (context.Context, context.CancelFunc) {
	return ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
