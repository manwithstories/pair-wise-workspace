package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/manwithstories/pair-wise-workspace/internal/fixture"
	"github.com/manwithstories/pair-wise-workspace/internal/forwarder"
	"github.com/manwithstories/pair-wise-workspace/internal/metrics"
	"github.com/manwithstories/pair-wise-workspace/internal/peer"
	rtpx "github.com/manwithstories/pair-wise-workspace/internal/rtp"
	"github.com/manwithstories/pair-wise-workspace/internal/signaling"
	"github.com/pion/rtp"
)

// Run executes one parsed command and returns the process exit code.
func Run(ctx context.Context, opts *Options, stdout, stderr io.Writer) int {
	switch opts.Command {
	case "serve":
		return runServe(ctx, opts, stdout, stderr)
	case "probe":
		return runProbe(ctx, opts, stdout, stderr)
	default:
		return runDemo(ctx, opts, stdout, stderr)
	}
}

// demoResult carries what the demo measured so the report and the threshold
// check are built from the same numbers.
type demoResult struct {
	report      metrics.Report
	problems    []string
	transport   error
	interrupted bool
}

func runDemo(ctx context.Context, opts *Options, stdout, stderr io.Writer) int {
	start := time.Now()
	res := executeDemo(ctx, opts, stderr)
	res.report.DurationMs = time.Since(start).Milliseconds()

	exit := ExitOK
	switch {
	case res.interrupted:
		// A cancelled run is not a transport failure and its metrics are a
		// fragment, so neither of the other codes would be honest.
		exit = ExitInterrupted
	case res.transport != nil:
		exit = ExitTransport
	case len(res.problems) > 0:
		exit = ExitThreshold
	}
	res.report.ExitCode = exit

	if opts.JSON {
		if err := res.report.Encode(stdout); err != nil {
			fmt.Fprintf(stderr, "encode report: %v\n", err)
			return ExitTransport
		}
	} else {
		if !opts.Quiet {
			fmt.Fprint(stdout, res.report.Human())
		}
		if res.transport != nil {
			fmt.Fprintf(stderr, "transport failure: %v\n", res.transport)
		}
		for _, p := range res.problems {
			fmt.Fprintf(stderr, "threshold failure: %s\n", p)
		}
	}
	return exit
}

// executeDemo is the whole data path: fixture -> forwarder -> sender peer ->
// receiver peer -> analyser. The signaling server is started and driven with
// the peers' real SDP, so a broken signaling path fails the demo rather than
// being decorative.
func executeDemo(ctx context.Context, opts *Options, stderr io.Writer) demoResult {
	plan, err := fixture.Build(opts.FixtureConfig())
	if err != nil {
		return demoResult{transport: fmt.Errorf("build fixture: %w", err)}
	}

	srv, err := signaling.Listen(opts.Addr, opts.Idle())
	if err != nil {
		return demoResult{transport: fmt.Errorf("signaling: %w", err)}
	}
	defer func() { _ = srv.Close() }()

	sigPair, err := exchangeSDP(srv.Addr(), opts)
	if err != nil {
		return demoResult{transport: err, report: metrics.Build(baseReport(plan, srv.Stats()))}
	}
	_ = sigPair.Close()

	pair, err := peer.Connect(peer.Config{
		ConnectTimeout: opts.ConnectTimeout(),
		MediaTimeout:   opts.MediaTimeout(),
		SSRC:           opts.SSRC,
	})
	if err != nil {
		return demoResult{transport: err, report: metrics.Build(baseReport(plan, srv.Stats()))}
	}
	defer func() { _ = pair.Close() }()

	return measure(ctx, opts, plan, pair, srv.Stats(), stderr)
}

// measure pumps the fixture through the pair and folds the counters into a
// report.
func measure(ctx context.Context, opts *Options, plan *fixture.Plan, pair *peer.Pair,
	sig signaling.Stats, stderr io.Writer) demoResult {

	analyzer := rtpx.NewAnalyzer(rtpx.Config{
		ClockRate:     plan.ClockRate,
		ReorderWindow: opts.Reorder,
	})

	// The forwarder sink writes to the sender track. The queue sits on the
	// send side, so a slow receiver shows up as backpressure drops rather than
	// as unbounded memory growth.
	var sendErr error
	fwd := forwarder.New(opts.Queue, func(p forwarder.Packet) error {
		if sendErr != nil {
			return sendErr
		}
		if err := pair.WritePacket(p.Raw); err != nil {
			sendErr = err
			return err
		}
		return nil
	})
	fwd.Start()

	readDone := make(chan error, 1)
	go func() {
		readDone <- pair.ReadLoop(opts.MediaTimeout(), func(pkt *rtp.Packet) error {
			analyzer.Observe(rtpx.Packet{
				SSRC:      pkt.SSRC,
				Sequence:  pkt.SequenceNumber,
				Timestamp: pkt.Timestamp,
				Marker:    pkt.Marker,
				Payload:   len(pkt.Payload),
				Arrival:   time.Now(),
			})
			return nil
		})
	}()

	writeErr := pump(ctx, plan, fwd)
	interrupted := ctx.Err() != nil
	// Let the tail land before reading counters.
	select {
	case <-ctx.Done():
	case <-time.After(opts.Drain()):
	}

	_ = fwd.Close()
	summary, sumErr := analyzer.Summary()
	readErr := stopRead(readDone, opts.MediaTimeout())
	if sendErr != nil && writeErr == nil {
		writeErr = sendErr
	}

	in := baseReport(plan, sig)
	in.StreamStats = summary
	in.Forwarder = fwd.Stats()
	in.LocalAddr, in.RemoteAddr = pair.SelectedPair()
	in.Loopback = in.LocalAddr != "" && pair.AllLoopback()
	in.Candidates = candidateLabels(pair)
	in.Rejected = pair.Rejected()
	report := metrics.Build(in)

	var transport error
	switch {
	case interrupted && errors.Is(writeErr, context.Canceled):
		// Cancellation is reported as its own outcome, not a transport fault.
	case writeErr != nil:
		transport = fmt.Errorf("send: %w", writeErr)
	case sumErr != nil && !interrupted:
		transport = fmt.Errorf("rtp stats: %w", sumErr)
	case readErr != nil && readErr != peer.ErrClosed && !interrupted:
		transport = fmt.Errorf("receive: %w", readErr)
	}
	if transport != nil {
		fmt.Fprintf(stderr, "demo: %v\n", transport)
	}

	// A cancelled run measured only a fragment of the stream, so threshold
	// violations would be noise rather than signal.
	problems := []string(nil)
	if !interrupted {
		problems = report.Check(metrics.DefaultThresholds(opts.Packets, opts.LossRate, opts.DuplicateRate))
	}
	return demoResult{
		report:      report,
		problems:    problems,
		transport:   transport,
		interrupted: interrupted,
	}
}

// baseReport seeds the parts of the metrics inputs known before media flows.
func baseReport(plan *fixture.Plan, sig signaling.Stats) metrics.Inputs {
	return metrics.Inputs{SourcePackets: plan.Source, Signaling: sig}
}

// pump walks the plan on a real clock, honouring ctx cancellation. The queue
// is bounded, so Push dropping a packet is reported through the forwarder's
// backpressure counter, not as an error here.
func pump(ctx context.Context, plan *fixture.Plan, fwd *forwarder.Forwarder) error {
	start := time.Now()
	for _, sp := range plan.Send {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := start.Add(time.Duration(sp.OffsetMs) * time.Millisecond)
		if wait := time.Until(target); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		raw, err := sp.Packet(plan.SSRC).Marshal()
		if err != nil {
			return fmt.Errorf("marshal packet %d: %w", sp.Index, err)
		}
		fwd.Push(forwarder.Packet{
			SSRC:      plan.SSRC,
			Sequence:  sp.Sequence,
			Timestamp: sp.Timestamp,
			Raw:       raw,
			Duplicate: sp.Duplicate,
		})
	}
	return nil
}

// candidateLabels formats gathered candidates for the report.
func candidateLabels(p *peer.Pair) []string {
	cands := p.Candidates()
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, fmt.Sprintf("%s:%s:%d", c.Side, c.Address, c.Port))
	}
	return out
}

// stopRead waits for the reader goroutine to report, but never hangs the exit.
func stopRead(done <-chan error, timeout time.Duration) error {
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return nil
	}
}
