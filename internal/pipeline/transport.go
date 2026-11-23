// RTP trace validation and bandwidth-adaptive batching.
package pipeline

import (
	"fmt"
	"path/filepath"
	"time"

	"archive.local/archive/internal/cache"
	"archive.local/archive/internal/fixture"
	"archive.local/archive/internal/model"
	"archive.local/archive/internal/repair"
	"archive.local/archive/internal/rtp"
)

func validateTransport(tracePath string) (TransportStats, error) {
	var out TransportStats
	if tracePath == "" {
		return out, nil
	}
	tr, err := fixture.ReadTrace(tracePath)
	if err != nil {
		return out, &model.Error{Code: model.CodeTransportDefect, Sample: -1, OffsetBytes: -1,
			Detail: "cannot decode transport trace", Err: err}
	}
	summary := rtp.NewTrack(tr.SSRC)
	for _, p := range tr.Packets {
		// Packets in the trace carry their wire bytes; decode them with
		// pion/rtp so validation uses the same codec as the recorder.
		if len(p.Wire) > 0 {
			decoded, derr := rtp.ParseWire(p.Wire)
			if derr != nil {
				return out, derr
			}
			p = decoded
		}
		summary.Push(p)
	}
	got := summary.Close()
	out.Packets = len(tr.Packets)
	out.Duplicate = got.Duplicate
	out.Reordered = got.Reordered
	out.GapRanges = len(got.Gaps)
	for _, g := range got.Gaps {
		out.BurstLoss += int(g.Count)
	}
	out.ConfirmedHigh = got.ConfirmedHigh
	out.MaxSeq = got.MaxSeq
	out.Wraps = got.Wraps
	// Adapt read batches to the observed delivery rates. Batches are sized
	// per loss-free run; a burst backs the next batch off without rewinding
	// the confirmed sequence high-water mark.
	est := rtp.NewEstimator(512, 1<<20, 512)
	var cumulativeLost int64
	var batchHit int
	window := time.Now()
	var windowBytes int64
	for i, a := range got.Arrivals {
		windowBytes += int64(len(a.Packet.Payload))
		// Fold a sample roughly every 100 arrivals, or at the tail.
		if (i+1)%100 != 0 && i != len(got.Arrivals)-1 {
			continue
		}
		elapsed := time.Since(window).Milliseconds()
		if elapsed < 1 {
			elapsed = 1
		}
		plan := est.Observe(rtp.BandwidthSample{WindowMs: elapsed, Bytes: windowBytes}, cumulativeLost)
		cumulativeLost = got.Lost
		window = time.Now()
		windowBytes = 0
		if plan.Packets > 0 {
			batchHit++
		}
	}
	out.Batches = batchHit
	return out, nil
}

type cacheResult struct{ hits, misses, rejected int }

// verifyCache re-verifies every pre-existing entry, then stores and hits one
// object per track. Any entry that no longer matches its provenance, length or
// digest is quarantined by the store and counted in rejected.
func verifyCache(work, manifestDigest string, order []uint32, tracks map[uint32][]repair.Sample,
	idx *repair.Index) (cacheResult, error) {
	var out cacheResult
	store, err := cache.NewStore(filepath.Join(work, "cache"))
	if err != nil {
		return out, err
	}
	if err := store.Rescan(); err != nil {
		return out, err
	}
	out.rejected = store.RejectedNow()
	out.hits, out.misses = store.Hits, store.Misses
	for _, id := range order {
		samples := tracks[id]
		if len(samples) == 0 {
			continue
		}
		last := samples[len(samples)-1]
		src := cache.Provenance(manifestDigest, id, samples[0].SourceTicks,
			samples[0].Offset, last.Offset+uint64(last.Size))
		payload := []byte(fmt.Sprintf("track=%d first=%d last=%d index=%s", id,
			samples[0].Unified, last.Unified, idx.Digest))
		e, err := store.Put(payload, src)
		if err != nil {
			return out, err
		}
		if _, _, err := store.Get(e.Address, src); err != nil {
			out.misses++
			continue
		}
		out.hits++
	}
	// Include any entry quarantined while rescanning or re-verifying.
	out.rejected = store.RejectedNow()
	out.hits, out.misses = store.Hits, store.Misses
	return out, nil
}
