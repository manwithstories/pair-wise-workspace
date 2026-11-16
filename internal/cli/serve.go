package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/manwithstories/pair-wise-workspace/internal/fixture"
	"github.com/manwithstories/pair-wise-workspace/internal/forwarder"
	"github.com/manwithstories/pair-wise-workspace/internal/metrics"
	rtpx "github.com/manwithstories/pair-wise-workspace/internal/rtp"
	"github.com/manwithstories/pair-wise-workspace/internal/signaling"
	"github.com/pion/rtp"
)

// runServe hosts the loopback signaling server until interrupted. It prints
// the bound address on stdout as JSON so a script can start it, read the port
// and drive a probe against it.
func runServe(ctx context.Context, opts *Options, stdout, stderr io.Writer) int {
	srv, err := signaling.Listen(opts.Addr, opts.Idle())
	if err != nil {
		fmt.Fprintf(stderr, "serve: %v\n", err)
		return ExitTransport
	}
	defer func() { _ = srv.Close() }()

	info := struct {
		Addr    string `json:"addr"`
		PID     int    `json:"pid"`
		IdleMs  int64  `json:"idle_ms"`
		Message string `json:"message"`
	}{
		Addr:    srv.Addr(),
		PID:     os.Getpid(),
		IdleMs:  opts.Idle().Milliseconds(),
		Message: "signaling server listening; send SIGINT to stop",
	}
	if opts.JSON {
		_ = json.NewEncoder(stdout).Encode(info)
	} else {
		fmt.Fprintf(stdout, "signaling listening on %s (pid %d)\n", info.Addr, info.PID)
	}
	if f, ok := stdout.(*os.File); ok {
		_ = f.Sync()
	}

	// Installing a handler here means Ctrl-C is a normal stop for `serve`,
	// not an interrupted run: the process reports its statistics and exits 0.
	// (ctx stays checked too, so a programmatic cancel also stops cleanly.)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	select {
	case <-ctx.Done():
	case <-sig:
	}
	_ = srv.Close()

	stats := srv.Stats()
	if opts.JSON {
		_ = json.NewEncoder(stdout).Encode(stats)
	} else {
		fmt.Fprintf(stdout, "signaling stopped: %d sessions, %d messages relayed, %d rejected\n",
			stats.SessionsCreated, stats.MessagesForwarded, stats.MessagesRejected)
	}
	return ExitOK
}

// runProbe is a self-check: it starts a signaling server, exchanges real SDP
// through it, sends a small deterministic fixture and asserts that every
// packet arrived. It is the fast smoke test for a machine.
func runProbe(ctx context.Context, opts *Options, stdout, stderr io.Writer) int {
	start := time.Now()
	report, err := probeOnce(ctx, opts)
	report.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		fmt.Fprintf(stderr, "probe: %v\n", err)
		if opts.JSON {
			report.ExitCode = ExitTransport
			_ = report.Encode(stdout)
		}
		return ExitTransport
	}
	exit := ExitOK
	problems := report.Check(probeThresholds(opts.ProbePackets))
	if len(problems) > 0 {
		exit = ExitThreshold
	}
	report.ExitCode = exit
	if opts.JSON {
		if err := report.Encode(stdout); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return ExitTransport
		}
	} else {
		fmt.Fprintf(stdout, "probe ok: %d/%d packets through the loopback path\n",
			report.ForwardedPackets, report.SourcePackets)
	}
	return exit
}

// probeOnce runs the smallest possible end-to-end exchange. By default it
// hosts its own signaling server; with --probe-addr it dials an already
// running `relay serve`, which is how a script checks a deployment.
func probeOnce(ctx context.Context, opts *Options) (metrics.Report, error) {
	var (
		srv   *signaling.Server
		addr  string
		stats = signaling.Stats{}
	)
	if opts.ProbeAddr != "" {
		// Probing someone else's server: its counters are not ours to read,
		// so signaling_* stays zero for this run.
		addr = opts.ProbeAddr
	} else {
		var err error
		srv, err = signaling.Listen(opts.Addr, opts.Idle())
		if err != nil {
			return metrics.Report{}, err
		}
		defer func() { _ = srv.Close() }()
		addr = srv.Addr()
	}
	statsOf := func() signaling.Stats {
		if srv == nil {
			return stats
		}
		return srv.Stats()
	}

	pair, err := exchangeSDP(addr, opts)
	if err != nil {
		return metrics.Build(baseReport(&fixture.Plan{Source: opts.ProbePackets}, statsOf())), err
	}
	defer func() { _ = pair.Close() }()

	cfg := opts.FixtureConfig()
	cfg.Packets = opts.ProbePackets
	cfg.LossRate = 0
	cfg.DuplicateRate = 0
	cfg.ReorderRate = 0
	cfg.ReorderWindow = 0
	plan, err := fixture.Build(cfg)
	if err != nil {
		return metrics.Report{}, err
	}

	analyzer := rtpx.NewAnalyzer(rtpx.Config{ClockRate: plan.ClockRate, ReorderWindow: opts.Reorder})
	fwd := forwarder.New(opts.Queue, func(p forwarder.Packet) error {
		return pair.WritePacket(p.Raw)
	})
	fwd.Start()

	readDone := make(chan error, 1)
	go func() {
		readDone <- pair.ReadLoop(opts.MediaTimeout(), func(pkt *rtp.Packet) error {
			analyzer.Observe(rtpx.Packet{
				SSRC: pkt.SSRC, Sequence: pkt.SequenceNumber, Timestamp: pkt.Timestamp,
				Marker: pkt.Marker, Payload: len(pkt.Payload), Arrival: time.Now(),
			})
			return nil
		})
	}()

	if err := pump(ctx, plan, fwd); err != nil {
		return metrics.Report{}, err
	}
	select {
	case <-ctx.Done():
	case <-time.After(opts.Drain()):
	}
	_ = fwd.Close()
	summary, sumErr := analyzer.Summary()
	_ = stopRead(readDone, opts.MediaTimeout())

	in := baseReport(plan, statsOf())
	if sumErr == nil {
		in.StreamStats = summary
	}
	in.Forwarder = fwd.Stats()
	in.LocalAddr, in.RemoteAddr = pair.SelectedPair()
	in.Loopback = in.LocalAddr != "" && pair.AllLoopback()
	in.Candidates = candidateLabels(pair)
	in.Rejected = pair.Rejected()
	report := metrics.Build(in)
	if sumErr != nil {
		return report, fmt.Errorf("rtp stats: %w", sumErr)
	}
	return report, nil
}

func probeThresholds(packets int) metrics.Thresholds {
	return metrics.Thresholds{
		MinForwarded: uint64(packets),
		MaxForwarded: uint64(packets),
		MaxJitterAvg: 20,
		RequirePairs: true,
	}
}
