package runner

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"rtprelay/internal/relay"
	"rtprelay/internal/report"
	"rtprelay/internal/rtpgen"
	"rtprelay/internal/stats"

	"github.com/pion/webrtc/v4"
)

func runDatapath(ctx context.Context, opts Options, base report.Report,
	localTrack *webrtc.TrackLocalStaticRTP, remoteTrack *webrtc.TrackRemote) Result {
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return fail(base, 4, fmt.Errorf("open loopback UDP: %w", err))
	}
	var udpPackets atomic.Uint64
	udpDone := make(chan struct{})
	go receiveUDP(udpConn, &udpPackets, udpDone)
	var closeOnce sync.Once
	stopUDP := func() {
		closeOnce.Do(func() {
			_ = udpConn.Close()
			<-udpDone
		})
	}
	defer stopUDP()

	target := udpConn.LocalAddr().(*net.UDPAddr)
	base.MaxJitterMS = opts.MaxJitterMS
	generator, err := rtpgen.New(rtpgen.Config{
		PPS: opts.PPS, Duration: opts.Duration, Seed: opts.Seed, Jitter: opts.Jitter,
		ClockRate: opts.ClockRate,
	})
	if err != nil {
		return fail(base, 2, err)
	}
	tracker := stats.NewTracker(opts.ClockRate)
	run, err := runPackets(ctx, localTrack, remoteTrack, generator, tracker, target, opts, uint64(generator.Count()))
	stopUDP()
	if err != nil {
		return fail(base, 4, err)
	}
	base.GeneratedPackets = run.Generated.Packets
	base.ReceivedPackets = run.Counts.Received
	base.ForwardedPackets = run.Counts.Forwarded
	base.UDPReceivedPackets = udpPackets.Load()
	base.Dropped = run.Counts.Dropped
	base.Duplicated = run.Counts.Duplicated
	base.Reordered = run.Counts.Reordered
	base.Stats = run.Stats
	base.SyncStats()

	if ctx.Err() != nil {
		base.OK = false
		base.Error = "interrupted"
		return Result{Report: base, ExitCode: 0}
	}
	if opts.MaxJitterMS > 0 && run.Stats.Jitter.MaxMs > opts.MaxJitterMS {
		return fail(base, 5, fmt.Errorf("jitter max %.3f ms exceeds %.3f ms",
			run.Stats.Jitter.MaxMs, opts.MaxJitterMS))
	}
	base.OK = true
	return Result{Report: base, ExitCode: 0}
}

type packetRun struct {
	Generated rtpgen.Result
	Counts    relay.Counts
	Stats     stats.Summary
}

func runPackets(ctx context.Context, localTrack *webrtc.TrackLocalStaticRTP,
	remoteTrack *webrtc.TrackRemote, generator *rtpgen.Generator,
	tracker *stats.Tracker, target *net.UDPAddr, opts Options, expected uint64) (packetRun, error) {
	packetLog := opts.PacketLog
	forwarder, err := relay.New(relay.Config{
		Target: target, Faults: opts.Faults, Seed: opts.Seed,
		PacketLog: packetLog, IdleTimeout: time.Second,
	})
	if err != nil {
		return packetRun{}, fmt.Errorf("create forwarder: %w", err)
	}
	defer forwarder.Close()
	forwardDone := make(chan error, 1)
	go func() {
		forwardDone <- forwarder.Run(ctx, remoteTrack, tracker, expected)
	}()
	time.Sleep(20 * time.Millisecond)
	genDone := make(chan struct{})
	var generated rtpgen.Result
	var genErr error
	go func() {
		generated, genErr = generator.Run(ctx, localTrack.WriteRTP)
		close(genDone)
	}()
	var forwardErr error
	select {
	case forwardErr = <-forwardDone:
	case <-ctx.Done():
		forwardErr = <-forwardDone
	}
	select {
	case <-genDone:
	case <-time.After(3 * time.Second):
		return packetRun{}, fmt.Errorf("generator did not stop")
	}
	if genErr != nil {
		return packetRun{}, fmt.Errorf("generate RTP: %w", genErr)
	}
	if forwardErr != nil {
		return packetRun{}, fmt.Errorf("forward RTP: %w", forwardErr)
	}
	time.Sleep(100 * time.Millisecond)
	return packetRun{
		Generated: generated, Counts: forwarder.Counts(), Stats: tracker.Summary(),
	}, nil
}

func receiveUDP(conn *net.UDPConn, count *atomic.Uint64, done chan<- struct{}) {
	defer close(done)
	buffer := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if n >= 12 && buffer[0]>>6 == 2 {
			count.Add(1)
		}
	}
}
