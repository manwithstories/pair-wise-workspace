package driver

import (
	"context"
	"io"
	"sort"

	"tsbatch/internal/fault"
	"tsbatch/internal/inspect"
	"tsbatch/internal/pipeline"
	"tsbatch/internal/repack"
	"tsbatch/internal/ts"
)

// Process exit codes.
const (
	ExitOK     = 0 // completed, results verifiable
	ExitIO     = 1 // input or output I/O failure
	ExitArgs   = 2 // argument error
	ExitStream = 3 // unrecoverable stream error
)

// GroupPackets is the maximum packet-group size; together with the
// queue capacity it bounds pipeline memory.
const GroupPackets = 256

// runResult aggregates one pipeline run.
type runResult struct {
	report   inspect.Report
	peak     int64
	packets  uint64
	readErr  error // input I/O failure (exit code 1)
	parseErr error // unrecoverable stream error (exit code 3)
}

// runStream drives the full chain: reader -> bounded worker pool ->
// deterministic merger -> analyzer -> repack writer. Backpressure
// travels back to the reader through the bounded job queue; ctx
// cancellation unwinds every stage.
func runStream(ctx context.Context, r io.Reader, w *repack.Writer,
	workers, queue, group int, maxSegPackets uint64) *runResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool := pipeline.NewPool(ctx, workers, queue)
	ordered := pipeline.Ordered(ctx, pool.Results())
	an := inspect.NewAnalyzer(w, maxSegPackets)
	res := &runResult{}

	var trunc *ts.TruncationError
	prodDone := make(chan struct{})
	go func() {
		defer close(prodDone)
		defer pool.Close()
		rd, err := ts.NewReader(r, ts.PacketSize)
		if err != nil {
			res.readErr = err
			cancel()
			return
		}
		var pkt [ts.PacketSize]byte
		var job pipeline.Job
		for {
			idx, err := rd.Next(&pkt)
			if err == io.EOF {
				break
			}
			if err != nil {
				if te, ok := err.(*ts.TruncationError); ok {
					trunc = te
					break
				}
				res.readErr = err
				cancel()
				return
			}
			if len(job.Packets) == 0 {
				job.FirstPacket = idx
			}
			job.Packets = append(job.Packets, pkt)
			res.packets++
			if len(job.Packets) == group {
				if pool.Submit(job) != nil {
					return
				}
				job = pipeline.Job{Seq: job.Seq + 1}
			}
		}
		if len(job.Packets) > 0 {
			pool.Submit(job)
		}
	}()

	for r := range ordered {
		if r.Err != nil && res.parseErr == nil {
			res.parseErr = r.Err
		}
		for _, ev := range r.Events {
			an.Consume(ev)
		}
		if w.Err() != nil {
			cancel() // downstream failure unwinds the whole chain
		}
	}
	<-prodDone

	last := uint64(0)
	if res.packets > 0 {
		last = res.packets - 1
	}
	rep := an.Finalize(last)
	if trunc != nil {
		rep.Faults = append(rep.Faults, fault.Fault{
			Category: fault.CatTruncation, PID: 0, PacketIndex: res.packets,
			MediaTime: fault.UnknownTime, Recoverable: true,
			Detail: trunc.Error(),
		})
	}
	sort.Sort(rep.Faults)
	res.report = rep
	res.peak = pool.Peak()
	return res
}
