// Package pipeline wires ingestion, timeline, media reconstruction, index
// and checkpoints into one bounded-memory restore run, and emits the
// metrics the archive verification stage consumes.
package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/archive/rtprestore/internal/checkpoint"
	"github.com/archive/rtprestore/internal/clock"
	"github.com/archive/rtprestore/internal/index"
	"github.com/archive/rtprestore/internal/rtp"
	"github.com/archive/rtprestore/internal/synth"
	"github.com/archive/rtprestore/internal/video"
)

// Metrics is the run summary printed by the CLI and checked by verify.
type Metrics struct {
	RecoverablePackets int
	RoundTripped       int
	RoundTripRate      float64
	KeyframeTruth      int
	KeyframeHits       int
	KeyframeHitRate    float64
	Segments           int
	SegmentSHA256Count int
	Gaps               []rtp.GapError
	SilenceFrames      int
	ClipFrames         int
	PreservedBoundary  int
	UncertainIntervals []clock.UncertainInterval
	ResumedShards      int
}

// Config is the pipeline-facing run configuration.
type Config struct {
	Params          checkpoint.Params
	WrapAt          uint64
	OutDir          string
	CheckpointEvery int
	BlockSize       int // max packets held before a block is finalized
}

// Run restores one synthetic capture. When resume is true it opens an
// existing checkpoint, reuses the intact shard prefix and skips the packet
// ticks already covered by that prefix so shards are never rewritten.
func Run(recs []synth.Record, truth synth.Truth, cfg Config, resume bool) (Metrics, *checkpoint.Store, error) {
	store, resumeCursor, err := openStore(cfg, resume)
	if err != nil {
		return Metrics{}, nil, err
	}
	reorder := rtp.NewReorderer(cfg.BlockSize)
	tl := clock.NewTimeline(cfg.WrapAt)
	vb := video.NewBuilder()
	ki := index.NewKeyframeIndexer()
	m := Metrics{}
	m.ResumedShards = len(store.State().Shards)
	block := newBlock()
	totalTicks := int64(0)
	// skipped marks packet ticks already represented by reused shards.
	skipped := int64(0)
	// Source-of-truth lookup keyed by SSRC+sequence so reordering never
	// confuses which generated packet an emitted packet corresponds to.
	source := map[[2]uint32]synth.Record{}
	for _, r := range recs {
		if r.Kind == 0 {
			source[[2]uint32{r.SSRC, uint32(r.Seq)}] = r
		}
	}

	for _, rec := range recs {
		if rec.Kind == 1 {
			sr := clock.SenderReport{SSRC: rec.SSRC, NTPTime: rec.NTPTime, RTPTimestamp: rec.Timestamp}
			if err := tl.ObserveSR(sr, rateFor(rec.SSRC)); err != nil {
				return m, nil, fmt.Errorf("pipeline: %w", err)
			}
			continue
		}
		pkt := toPacket(rec)
		emitted, perr := reorder.Push(pkt)
		if perr != nil {
			return m, nil, fmt.Errorf("pipeline: %w", perr)
		}
		for _, op := range emitted {
			totalTicks++
			// On resume, packet ticks already covered by reused shards are
			// replayed through the timeline (so drift/unwrap state stays
			// continuous) and still counted for metrics, but not
			// re-serialized into a new shard.
			if resumeCursor > 0 && skipped < resumeCursor {
				skipped++
				if _, err := tl.Map(op.SSRC, op.Seq, op.Packet.Timestamp, rateFor(op.SSRC)); err != nil {
					return m, nil, fmt.Errorf("pipeline: %w", err)
				}
				m.RecoverablePackets++
				if src, ok := source[[2]uint32{op.SSRC, uint32(op.Seq)}]; ok && roundTripEqual(src, op) {
					m.RoundTripped++
				}
				if op.SSRC == synth.VideoSSRC {
					frame, ferr := vb.Push(video.PacketInput{
						Tick: op.Tick, SSRC: op.SSRC, Seq: op.Seq,
						Timestamp: op.Packet.Timestamp, Marker: op.Packet.Marker,
						Payload: op.Packet.Payload,
					})
					if ferr != nil {
						return m, nil, fmt.Errorf("pipeline: %w", ferr)
					}
					if frame != nil {
						ki.Add(index.FrameView{
							ID: frame.ID, SSRC: frame.SSRC, PTSUs: frame.PTSUs,
							Keyframe: frame.Keyframe, Bad: frame.Bad, FirstTick: frame.FirstTick,
						})
					}
				}
				continue
			}
			st, err := tl.Map(op.SSRC, op.Seq, op.Packet.Timestamp, rateFor(op.SSRC))
			if err != nil {
				return m, nil, fmt.Errorf("pipeline: %w", err)
			}
			m.RecoverablePackets++
			block.addPacket(op, st)
			if src, ok := source[[2]uint32{op.SSRC, uint32(op.Seq)}]; ok && roundTripEqual(src, op) {
				m.RoundTripped++
			}
			if op.SSRC == synth.VideoSSRC {
				frame, ferr := vb.Push(video.PacketInput{
					Tick: op.Tick, SSRC: op.SSRC, Seq: op.Seq,
					Timestamp: op.Packet.Timestamp, Marker: op.Packet.Marker,
					Payload: op.Packet.Payload,
				})
				if ferr != nil {
					return m, nil, fmt.Errorf("pipeline: %w", ferr)
				}
				if frame != nil {
					ki.Add(index.FrameView{
						ID: frame.ID, SSRC: frame.SSRC, PTSUs: frame.PTSUs,
						Keyframe: frame.Keyframe, Bad: frame.Bad, FirstTick: frame.FirstTick,
					})
					block.addFrame(frame)
				}
			} else {
				block.queueAudio(op)
			}
			if block.len() >= cfg.BlockSize {
				if err := finalizeBlock(store, block, ki, tl, cfg, totalTicks); err != nil {
					return m, nil, err
				}
				s, c, p := block.audioCounters()
				m.SilenceFrames += s
				m.ClipFrames += c
				m.PreservedBoundary += p
				block = newBlock()
			}
		}
	}
	// Drain audio normalization on the remaining bounded fragment.
	flushAudio(block)
	// Close any frames still waiting for a marker at end of stream. Frames
	// already serialized into a shard were indexed inline; Flush only adds
	// the index entries that had no closing marker, never a new shard.
	for _, frame := range vb.Flush() {
		ki.Add(index.FrameView{
			ID: frame.ID, SSRC: frame.SSRC, PTSUs: frame.PTSUs,
			Keyframe: frame.Keyframe, Bad: frame.Bad, FirstTick: frame.FirstTick,
		})
	}
	if block.len() > 0 {
		if err := finalizeBlock(store, block, ki, tl, cfg, totalTicks); err != nil {
			return m, nil, err
		}
		s, c, p := block.audioCounters()
		m.SilenceFrames += s
		m.ClipFrames += c
		m.PreservedBoundary += p
	}
	_, gaps := reorder.Flush()
	m.Gaps = gaps
	m.UncertainIntervals = tl.UncertainIntervals()
	idx := ki.Result()
	m.KeyframeTruth = len(truth.KeyframeIDs)
	m.KeyframeHits = countHits(idx, truth)
	m.KeyframeHitRate = safeRate(m.KeyframeHits, m.KeyframeTruth)
	m.RoundTripRate = safeRate(m.RoundTripped, m.RecoverablePackets)
	m.Segments = len(store.State().Shards)
	m.SegmentSHA256Count = m.Segments
	if err := store.Finalize(totalTicks, tl.Version()); err != nil {
		return m, nil, err
	}
	return m, store, nil
}

func openStore(cfg Config, resume bool) (*checkpoint.Store, int64, error) {
	if resume {
		store, st, err := checkpoint.Resume(cfg.OutDir, cfg.CheckpointEvery, cfg.Params)
		if err != nil {
			return nil, 0, err
		}
		return store, st.Cursor, nil
	}
	store, err := checkpoint.Open(cfg.OutDir, cfg.CheckpointEvery, cfg.Params)
	return store, 0, err
}

func rateFor(ssrc uint32) uint32 {
	if ssrc == synth.VideoSSRC {
		return synth.VideoRate
	}
	return synth.AudioRate
}

func safeRate(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func countHits(x index.Index, truth synth.Truth) int {
	have := map[uint32]bool{}
	for _, e := range x.Entries {
		have[e.FrameID] = true
	}
	hits := 0
	for _, id := range truth.KeyframeIDs {
		if truth.BadFrameIDs[id] {
			continue
		}
		if have[id] {
			hits++
		}
	}
	return hits
}

// roundTripEqual confirms the rewritten packet preserves the identity
// fields of the generated source: SSRC, sequence, timestamp and payload.
func roundTripEqual(src synth.Record, op rtp.OrderedPacket) bool {
	if src.SSRC != op.SSRC || src.Seq != op.Seq || src.Timestamp != op.Packet.Timestamp {
		return false
	}
	return bytes.Equal(src.Payload, op.Packet.Payload)
}

// hashBytes is shared so segment and evidence hashing stay consistent.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
