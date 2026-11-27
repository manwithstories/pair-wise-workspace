package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tsqc/internal/clock"
	"tsqc/internal/fixture"
	"tsqc/internal/rtp"
)

// Run executes generation, recovery, demux, segmentation, caching and
// checkpointing in one pass. Deterministic inputs produce identical digests.
func Run(opts RunOptions) (*Report, error) {
	source := SourceID(opts.Seed, opts.Faults)
	st, err := fixture.Generate(fixture.Options{
		Seed: opts.Seed, IDRs: opts.IDRs, GOP: opts.GOP, AudioPerGOP: opts.Audio,
		Faults: opts.Faults,
	})
	if err != nil {
		return nil, err
	}
	rec := rtp.NewReceiver()
	for _, g := range st.Groups {
		if err := rec.AddFEC(rtp.FECBlock{Base: g.Base, Count: g.Count, Lengths: g.Lengths, Parity: g.Parity}); err != nil {
			return nil, err
		}
	}
	// Feed with a synthetic arrival clock and a bandwidth estimate so the
	// jitter window adapts within its bounded range. Arrivals advance 2 ms per
	// packet, longer than the default wait, so a packet a few places out of
	// order is still held until its predecessor shows up or the wait expires.
	var recovered []byte
	emit := func(batch []rtp.Packet) {
		for _, q := range batch {
			recovered = append(recovered, q.Payload...)
		}
	}
	start := time.Duration(0)
	for i, p := range st.Packets {
		rec.SetBandwidth(int64(len(p.Payload)) * 8 * 1000)
		arrival := start + time.Duration(i)*2*time.Millisecond
		// Decode through Pion exactly as an on-wire receiver would.
		out, _, err := rec.PushWire(p.Raw, arrival)
		if err != nil {
			return nil, err
		}
		emit(out)
		emit(rec.Reorderer().Advance(arrival))
	}
	tail, _ := rec.Flush()
	emit(tail)
	stats := rec.Stats()

	d, aus, demuxErr := demuxStream(recovered)
	if demuxErr != nil && !clock.IsRecoverable(demuxErr) {
		return nil, demuxErr
	}
	aus = append(aus, d.Flush()...)
	pmt := d.Tables().ActivePMT()
	ix := BuildIndex(aus, pmt)
	videoPID, audioPID := ix.VideoPID, ix.AudioPID
	if videoPID == 0 {
		videoPID = fixture.VideoPID
	}
	if audioPID == 0 {
		audioPID = fixture.AudioPID
	}
	segs, err := DefaultSegmenter(videoPID, audioPID).Build(aus)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return &Report{
			Source: source, Seed: opts.Seed, Faults: opts.Faults, Fatal: true,
			RTPPackets: len(st.Packets), TSBytes: len(recovered),
			Recovered: stats.Recovered, Unrecoverable: stats.Unrecoverable,
		}, nil
	}
	passed := 0
	for i := range segs {
		if err := VerifySegment(&segs[i]); err != nil {
			return nil, err
		}
		passed++
	}
	gaps := collectGaps(aus)

	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return nil, err
	}
	store, err := OpenStore(filepath.Join(opts.State, "cas"), 32<<20)
	if err != nil {
		return nil, err
	}
	cpPath := CheckpointPath(opts.State)
	cp, cpErr := LoadCheckpoint(cpPath)
	if cpErr != nil {
		// Corrupt or stale checkpoints are discarded; work is recomputed.
		cp = nil
	}
	if cp == nil || !cp.Compatible(source, digestOf(recovered)) {
		cp = &Checkpoint{RunID: source, InputDigest: digestOf(recovered)}
	}
	rep := &Report{
		Source: source, Seed: opts.Seed, Faults: st.Faults,
		InputSHA:   digestOf(recovered),
		RTPPackets: len(st.Packets), TSBytes: len(recovered),
		Recovered: stats.Recovered, Unrecoverable: stats.Unrecoverable,
		Segments: len(segs), Gaps: gaps,
	}
	if cpErr != nil {
		rep.Notes = append(rep.Notes, "checkpoint discarded: "+cpErr.Error())
	}
	for i := range segs {
		seg := &segs[i]
		file := fmt.Sprintf("seg-%02d.ts", seg.Index)
		key := CacheKey{Source: source, InputDigest: seg.InputSHA, Segment: seg.Index}
		if cached, ok := store.Get(key); ok && equalBytes(cached, seg.Bytes) {
			rep.CacheHits++
		} else {
			rep.CacheMisses++
			if _, err := store.Put(key, seg.Bytes); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(filepath.Join(opts.Out, file), seg.Bytes, 0o644); err != nil {
			return nil, err
		}
		if cp.Commit(seg) {
			rep.CheckpointSaved++
			if err := cp.Save(cpPath); err != nil {
				return nil, err
			}
		} else {
			rep.ResumedSegments++
		}
		rep.Digests = append(rep.Digests, seg.Digest())
		rep.ContentDigests = append(rep.ContentDigests, seg.ContentSHA)
	}
	if err := cp.Save(cpPath); err != nil {
		return nil, err
	}
	rep.Roundtrip = float64(passed) * 100 / float64(len(segs))
	rep.KeyframeHitRate = ix.KeyframeHitRate(segs)
	if err := writeManifest(opts.Out, rep, segs); err != nil {
		return nil, err
	}
	return rep, nil
}
