package main

import (
	"context"
	"io"
	"sort"
	"sync"
	"time"

	"archive/internal/stream"
)

// transportOut carries the RTP verification metrics.
type transportOut struct {
	stats    stream.RTPStats
	p95ms    float64
	pipePeak int64
	packetN  int
	payloadB int
}

// planSequence builds the deterministic packet order: the base stream
// skips the loss burst at [100, 100+burst); all but the last lost packet
// are retransmitted inside the reorder window, the last one far too late.
// One duplicate (seq 200) is injected as well.
func planSequence(n, burst int) []uint16 {
	lost := func(s int) bool { return s >= 100 && s < 100+burst }
	var seqs []uint16
	for s := 0; s < n; s++ {
		if lost(s) {
			continue
		}
		seqs = append(seqs, uint16(s))
		if s == 200 { // duplicate injection
			seqs = append(seqs, 200)
		}
		if s == 100+burst+2 { // in-window retransmissions
			for k := 0; k < burst-1; k++ {
				seqs = append(seqs, uint16(100+k))
			}
		}
	}
	seqs = append(seqs, uint16(100+burst-1)) // last lost packet: too late
	return seqs
}

// runTransport pushes a fixed-length RTP sequence through the bounded
// pipe to a slow consumer and measures per-packet delivery latency. The
// reorder window is 8; loss recovery is verified by the reorderer stats.
func runTransport(ctx context.Context, opts demoOpts) (*transportOut, error) {
	const (
		n       = 400 // fixed-length sequence
		payload = 1200
		window  = 8
	)
	byteLimit := int64(opts.memoryMiB) << 20 / 4 // data-plane share of budget
	pipe := stream.NewPipe(byteLimit, 4)
	re := stream.NewReorderer(window)
	seqs := planSequence(n, opts.lossBurst)
	sentAt := make([]time.Time, n)
	var mu sync.Mutex
	var lat []float64 // milliseconds

	prodDone := make(chan error, 1)
	go func() {
		for _, s := range seqs {
			b, err := stream.EncodePacket(s, uint32(s)*3000, make([]byte, payload))
			if err != nil {
				pipe.Close(err)
				prodDone <- err
				return
			}
			mu.Lock()
			sentAt[s] = time.Now()
			mu.Unlock()
			if err := pipe.Send(ctx, b); err != nil {
				pipe.Close(err)
				prodDone <- err
				return
			}
		}
		pipe.Close(nil)
		prodDone <- nil
	}()

	delay := time.Duration(opts.consumerDelayMs) * time.Millisecond
	for {
		b, err := pipe.Recv(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		p, err := stream.DecodePacket(b)
		if err != nil {
			return nil, err
		}
		for _, q := range re.Push(p) {
			mu.Lock()
			lat = append(lat, float64(time.Since(sentAt[q.SequenceNumber]).Microseconds())/1000)
			mu.Unlock()
		}
		time.Sleep(delay) // slow consumer: forces backpressure
	}
	for _, q := range re.Flush() {
		mu.Lock()
		lat = append(lat, float64(time.Since(sentAt[q.SequenceNumber]).Microseconds())/1000)
		mu.Unlock()
	}
	if err := <-prodDone; err != nil {
		return nil, err
	}
	return &transportOut{
		stats: re.Stats, p95ms: percentile95(lat), pipePeak: pipe.Peak(),
		packetN: n, payloadB: payload,
	}, nil
}

// percentile95 returns the 95th percentile of latencies in milliseconds.
func percentile95(lat []float64) float64 {
	if len(lat) == 0 {
		return 0
	}
	s := append([]float64(nil), lat...)
	sort.Float64s(s)
	idx := (95*len(s) + 99) / 100 // ceil(0.95*n)
	return s[idx-1]
}
