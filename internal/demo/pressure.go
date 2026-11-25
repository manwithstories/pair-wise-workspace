package demo

import (
	"context"
	"math/rand"
	"time"

	"github.com/pion/rtp"

	"fieldpost/internal/core"
	"fieldpost/pipeline"
)

// PressureResult contains bounded-memory observability.
type PressureResult struct {
	Snapshot pipeline.Snapshot
}

// RunPressure submits more actual byte buffers than the cap while one job is
// held open, deterministically producing backpressure. It then releases the
// job and waits for every worker, proving no goroutine is stranded.
func RunPressure(parent context.Context, capBytes int64) (pipeline.Snapshot, error) {
	chunk := int64(768 << 10)
	if capBytes < chunk*2 {
		chunk = capBytes / 4
	}
	if chunk <= 0 {
		chunk = 4096
	}
	started := make(chan struct{}, 1)
	hold := make(chan struct{})
	p := pipeline.New(parent, capBytes, 1, 64, func(_ context.Context, item pipeline.Item) error {
		if item.ID == "pressure-00" {
			started <- struct{}{}
			<-hold
		}
		// Touch both ends so the compiler cannot optimize the buffer away.
		if len(item.Data) > 0 {
			item.Data[0] ^= 0x5a
			item.Data[len(item.Data)-1] ^= 0xa5
		}
		return nil
	})

	firstData := make([]byte, chunk)
	if err := p.Submit(parent, pipeline.Item{ID: "pressure-00", Weight: chunk, Data: firstData}); err != nil {
		return p.Snapshot(), err
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(hold)
		_ = p.Close()
		return p.Snapshot(), context.DeadlineExceeded
	}

	// With pressure-00 resident, this many extras is guaranteed to make at
	// least one Reserve call wait regardless of goroutine scheduling.
	extra := int(capBytes/chunk) + 2
	if extra < 4 {
		extra = 4
	}
	results := make(chan error, extra)
	for i := 1; i <= extra; i++ {
		data := make([]byte, chunk)
		id := "pressure-" + twoDigit(i)
		go func() {
			results <- p.Submit(parent, pipeline.Item{ID: id, Weight: chunk, Data: data})
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && p.Snapshot().BackpressureEvents == 0 {
		time.Sleep(time.Millisecond)
	}
	if p.Snapshot().BackpressureEvents == 0 {
		close(hold)
		for i := 0; i < extra; i++ {
			<-results
		}
		_ = p.Close()
		return p.Snapshot(), context.DeadlineExceeded
	}
	close(hold)
	var firstErr error
	for i := 0; i < extra; i++ {
		if err := <-results; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return p.Snapshot(), firstErr
	}
	if err := p.Close(); err != nil {
		return p.Snapshot(), err
	}
	snap := p.Snapshot()
	if snap.PeakInFlight > capBytes {
		return snap, core.InvariantError(core.StagePipeline, "memory-gate", snap.PeakInFlight,
			"peak in-flight %d exceeds cap %d", snap.PeakInFlight, capBytes)
	}
	return snap, nil
}

func twoDigit(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func impairPackets(seed int64, packets []*rtp.Packet, omit int) []*rtp.Packet {
	if omit < 0 || omit >= len(packets) {
		omit = len(packets) / 2
	}
	var out []*rtp.Packet
	for i, p := range packets {
		if i == omit {
			continue
		}
		out = append(out, p)
		if i == 1 {
			out = append(out, p) // deterministic duplicate
		}
	}
	r := rand.New(rand.NewSource(seed ^ 0x1234abcd))
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
