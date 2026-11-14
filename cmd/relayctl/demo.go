package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"relayctl/internal/fixture"
	"relayctl/internal/peer"
	"relayctl/internal/relay"
	"relayctl/internal/signal"
)

type demoConfig struct {
	packets      int
	duration     time.Duration
	seed         int64
	impair       fixture.Impairment
	sink         string
	bind         string
	jsonPath     string
	externalSink bool
	ssrc         uint32
	startSeq     uint16
}

func runDemo(args []string, stdout io.Writer) error {
	fs := newFlagSet("demo", stdout)
	packets := fs.Int("packets", 3000, "number of generated packets")
	duration := fs.Duration("duration", 6*time.Second, "media schedule duration")
	seed := fs.Int64("seed", 42, "fixture PRNG seed")
	impairText := fs.String("impair", "", "loss=0.01,dup=0.01,reorder=0.02")
	sinkAddr := fs.String("sink", "127.0.0.1:0", "internal loopback sink bind or external sink target")
	bind := fs.String("bind", "127.0.0.1:0", "loopback signalling bind address")
	jsonPath := fs.String("json", "", "JSON report path")
	externalSink := fs.Bool("external-sink", false, "target an already-running sink instead of binding one")
	ssrc := fs.Uint64("ssrc", 0x524C5941, "rewritten downstream SSRC")
	startSeq := fs.Uint("start-seq", 0x4000, "rewritten downstream starting sequence")
	if err := fs.Parse(args); err != nil {
		return withCode("bad_args", "see relayctl demo -h", err)
	}
	if fs.NArg() != 0 {
		return withCode("bad_args", "unexpected demo arguments", errors.New(fs.Args()[0]))
	}
	if *packets <= 0 || *duration <= 0 {
		return withCode("bad_args", "packets and duration must be positive", fmt.Errorf("packets=%d duration=%s", *packets, *duration))
	}
	if *ssrc > uint64(^uint32(0)) || *startSeq > 65535 {
		return withCode("bad_args", "ssrc/start-seq out of range", errors.New("32-bit or 16-bit overflow"))
	}
	if err := validateLoopbackAddr(*bind); err != nil {
		return err
	}
	if err := validateLoopbackAddr(*sinkAddr); err != nil {
		return err
	}
	impair, err := fixture.ParseImpairment(*impairText)
	if err != nil {
		return withCode("bad_args", "use loss/dup/reorder probabilities", err)
	}
	generated, err := fixture.Generate(fixture.Options{
		Packets: *packets, Duration: *duration, Seed: *seed, Impairment: impair,
		StartSequence: 65500, StartTimestamp: 0xFFFF0000,
	})
	if err != nil {
		return withCode("bad_args", "reduce packets or increase duration", err)
	}
	report, err := executeDemo(context.Background(), demoConfig{
		packets: *packets, duration: *duration, seed: *seed, impair: impair,
		sink: *sinkAddr, bind: *bind, jsonPath: *jsonPath,
		externalSink: *externalSink, ssrc: uint32(*ssrc), startSeq: uint16(*startSeq),
	}, generated, stdout)
	if err != nil {
		return err
	}
	if report.ForwardErrors > 0 {
		return withCode("sink_unavailable", "start relayctl sink or use --sink 127.0.0.1:0", fmt.Errorf("%d downstream writes failed", report.ForwardErrors))
	}
	return nil
}

func executeDemo(parent context.Context, cfg demoConfig, generated fixture.Generated, stdout io.Writer) (relay.Report, error) {
	baseCtx, cancelCause := context.WithCancelCause(parent)
	ctx, timeoutCancel := context.WithTimeout(baseCtx, cfg.duration+30*time.Second)
	defer timeoutCancel()
	defer cancelCause(nil)
	stats := relay.NewStats()
	stats.SetEmitted(uint64(cfg.packets))

	server, err := signal.Listen(cfg.bind, signal.Options{})
	if err != nil {
		return relay.Report{}, fmt.Errorf("start demo signalling: %w", err)
	}
	defer server.Close()
	if cfg.externalSink {
		if err := relay.CheckSink(ctx, cfg.sink); err != nil {
			return relay.Report{}, withCode("sink_unavailable", "start relayctl sink or omit --external-sink", err)
		}
	}

	var sink *relay.Sink
	var sinkDone chan struct{}
	var sinkRunDone chan error
	target := cfg.sink
	if !cfg.externalSink {
		sink, err = relay.ListenSink(cfg.sink)
		if err != nil {
			return relay.Report{}, fmt.Errorf("start demo sink: %w", err)
		}
		target = sink.Addr()
		sinkDone = make(chan struct{})
		sinkRunDone = make(chan error, 1)
		go func() { sinkRunDone <- sink.Run(sinkDone) }()
		defer func() {
			close(sinkDone)
			_ = sink.Close()
			<-sinkRunDone
		}()
	}

	sessionID := fmt.Sprintf("demo-%d", time.Now().UnixNano())
	relayClient, senderClient, relayPeer, senderPeer, err := openDemoPeers(ctx, server.Addr().String(), sessionID)
	if err != nil {
		return relay.Report{}, err
	}
	defer senderClient.Close()
	defer relayClient.Close()
	defer senderPeer.Close()
	defer relayPeer.Close()
	if err := senderPeer.AddVideoTrack("demo-video", "demo"); err != nil {
		return relay.Report{}, err
	}
	forwarder, err := relay.NewUDPForwarder(target, cfg.ssrc, cfg.startSeq, stats)
	if err != nil {
		return relay.Report{}, fmt.Errorf("start UDP forwarder: %w", err)
	}
	defer forwarder.Close()

	errCh := make(chan error, 8)
	fail := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
		cancelCause(err)
	}
	senderReady := make(chan struct{})
	relayReady := make(chan struct{})
	demoCandidateSink(senderClient, senderPeer, fail)
	demoCandidateSink(relayClient, relayPeer, fail)
	go func() { fail(demoSenderSignaling(ctx, senderClient, senderPeer, senderReady, fail)) }()
	go func() { fail(demoRelaySignaling(ctx, relayClient, relayPeer, relayReady, fail)) }()
	if err := waitDemoReady(ctx, errCh, senderReady, relayReady); err != nil {
		return relay.Report{}, err
	}

	rtpCh := make(chan *rtp.Packet, 1024)
	stopPipeline := make(chan struct{})
	pipelineDone := make(chan error, 1)
	go func() { pipelineDone <- runRelayPipeline(ctx, rtpCh, stopPipeline, stats, forwarder) }()
	go readDemoTrack(ctx, relayPeer, rtpCh, fail)
	if err := sendFixtureAtPace(ctx, senderPeer, generated.Events); err != nil {
		return relay.Report{}, err
	}
	if err := sleepContext(ctx, 800*time.Millisecond); err != nil {
		return relay.Report{}, err
	}
	close(stopPipeline)
	if err := <-pipelineDone; err != nil {
		return relay.Report{}, err
	}
	if err := ctx.Err(); err != nil {
		return relay.Report{}, demoContextError(ctx)
	}
	if err := sleepContext(ctx, 30*time.Millisecond); err != nil {
		return relay.Report{}, err
	}

	selected, ok := senderPeer.SelectedPair()
	if !ok {
		selected, _ = relayPeer.SelectedPair()
	}
	_, _, bannedSender := senderPeer.CandidateSnapshot()
	_, _, bannedRelay := relayPeer.CandidateSnapshot()
	banned := server.BannedCandidates() + bannedSender + bannedRelay
	metrics := stats.Snapshot()
	selectedText := selected.Local
	report := relay.NewReport(cfg.seed, cfg.packets, cfg.duration, generated.ClockRate, generated.StartSeq, target, selectedText, banned, metrics)
	report.StartSequence = cfg.startSeq
	report.SourceSequence = generated.StartSeq
	report.ForwardSSRC = cfg.ssrc
	if sink != nil {
		packets, bytes := sink.Counts()
		report.SinkPackets = packets
		report.SinkBytes = bytes
	}
	if err := relay.WriteReport(cfg.jsonPath, report); err != nil {
		return relay.Report{}, err
	}
	printDemoMetrics(stdout, report)
	_ = senderClient.Send(&signal.Message{Type: signal.TypeBye})
	_ = relayClient.Send(&signal.Message{Type: signal.TypeBye})
	if banned > 0 {
		return relay.Report{}, fmt.Errorf("%w: banned_candidates=%d", signal.ErrNonLoopbackCandidate, banned)
	}
	return report, nil
}

func printDemoMetrics(w io.Writer, r relay.Report) {
	fmt.Fprintf(w, "emitted=%d received=%d unique=%d forwarded=%d duplicates=%d reordered=%d late_dropped=%d loss_ratio=%.4f forward_bytes=%d jitter_ms p50=%.3f p95=%.3f max=%.3f selected=%s sink_packets=%d\n",
		r.Emitted, r.Received, r.Unique, r.Forwarded, r.Duplicates, r.Reordered, r.LateDropped,
		r.LossRatio, r.ForwardBytes, r.JitterMS.P50, r.JitterMS.P95, r.JitterMS.Max, r.SelectedPair, r.SinkPackets)
}

func sendFixtureAtPace(ctx context.Context, session *peer.Session, events []fixture.Scheduled) error {
	start := time.Now()
	for _, event := range events {
		wait := time.Until(start.Add(event.At))
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return demoContextError(ctx)
			}
		}
		if err := session.WriteRTP(event.Packet); err != nil {
			return withCode("rtp_write_failed", "media transport closed during fixture playback", err)
		}
	}
	return nil
}

func readDemoTrack(ctx context.Context, session *peer.Session, out chan<- *rtp.Packet, fail func(error)) {
	var track *webrtc.TrackRemote
	select {
	case track = <-session.RemoteTrack():
	case <-ctx.Done():
		return
	}
	err := peer.ReadRemoteRTP(ctx, track, out)
	if err != nil && ctx.Err() == nil {
		fail(withCode("rtp_read_failed", "inspect relay receiver media transport", err))
	}
}

func demoContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", signal.ErrSignalTimeout, context.Cause(ctx))
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return ctx.Err()
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return demoContextError(ctx)
	}
}
