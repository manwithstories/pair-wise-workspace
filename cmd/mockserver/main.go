package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"rtspaudit/internal/mockserver"
)

func main() {
	cfg := mockserver.Config{}
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:0", "loopback listen address")
	flag.IntVar(&cfg.Frames, "frames", 120, "generated frame count")
	flag.Float64Var(&cfg.Loss, "loss", 0.02, "packet loss probability")
	flag.Float64Var(&cfg.Reorder, "reorder", 0.01, "adjacent reorder probability")
	flag.Float64Var(&cfg.Duplicate, "dup", 0.01, "duplicate probability")
	flag.Int64Var(&cfg.Seed, "seed", 7, "deterministic impairment seed")
	flag.StringVar(&cfg.Policy, "policy", "strict", "strict or lenient")
	flag.DurationVar(&cfg.SessionTimeout, "session-timeout", 0, "session timeout")
	flag.IntVar(&cfg.StallAfter, "stall-after", 0, "packets before a deliberate stall")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	if err := validate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "argument error:", err)
		os.Exit(2)
	}
	server, err := mockserver.Start(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start mock server:", err)
		os.Exit(1)
	}
	defer server.Close()
	fmt.Printf("url=%s frames=%d loss=%.3f reorder=%.3f dup=%.3f seed=%d policy=%s\n",
		server.URL(), cfg.Frames, cfg.Loss, cfg.Reorder, cfg.Duplicate, cfg.Seed, cfg.Policy)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

func validate(cfg mockserver.Config) error {
	if cfg.Frames <= 0 {
		return fmt.Errorf("frames must be positive")
	}
	for name, value := range map[string]float64{
		"loss": cfg.Loss, "reorder": cfg.Reorder, "dup": cfg.Duplicate,
	} {
		if value < 0 || value > 1 {
			return fmt.Errorf("%s must be in 0..1", name)
		}
	}
	if cfg.Policy != "strict" && cfg.Policy != "lenient" {
		return fmt.Errorf("policy must be strict or lenient")
	}
	if cfg.SessionTimeout < 0 || cfg.StallAfter < 0 || cfg.StallAfter > cfg.Frames {
		return fmt.Errorf("invalid timeout or stall-after")
	}
	return nil
}
