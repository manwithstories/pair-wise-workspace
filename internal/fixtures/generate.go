// Package fixtures generates every input the demo and the end-to-end
// tests use. Nothing here touches the network: all manifests, segment
// bytes and RTP datagrams are synthesized locally from a seed, and the
// generator is deterministic so a fixed seed always yields identical
// bytes and identical index digests.
package fixtures

import (
	"fmt"
	"math/rand"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/ingest"
	"github.com/pion/rtp"
)

// Case selects which fault scenario the generator injects.
type Case string

const (
	CaseNormal             Case = "normal"
	CaseLoss               Case = "loss"
	CaseDuplicate          Case = "duplicate"
	CaseLateOutOfOrder     Case = "late-out-of-order"
	CaseTimestampWrap      Case = "timestamp-wrap"
	CaseTruncatedBox       Case = "truncated-box"
	CaseLateOutsideWindow  Case = "late-outside-window"
)

// Config controls one generated capture.
type Config struct {
	Seed          int64
	DurationTicks clock.Ticks
	FrameStep     clock.Ticks // RTP timestamp increment per frame
	SegmentStep   clock.Ticks // fMP4 segment duration (coarser than frames)
	LossRate      float64     // fraction of packets dropped (0..1)
	DuplicateRate float64
	LateRate      float64
	Wrap          bool
	TruncateBox   bool
	// LateOutside pushes one late event beyond the replay window.
	LateOutside bool
}

// DefaultConfig returns the demo's baseline settings.
func DefaultConfig(seed int64) Config {
	return Config{
		Seed:          seed,
		DurationTicks: clock.Ticks(300 * clock.Rate),
		FrameStep:     clock.Ticks(3600),                // 40 ms at 90 kHz
		SegmentStep:   clock.Ticks(2 * clock.Rate),      // 2 s fMP4 segments
		LossRate:      0.02,
	}
}

// Packet is a generated RTP datagram with its injection metadata.
type Packet struct {
	Raw       []byte
	Seq       uint16
	TS        uint32
	IsRTX     bool
	IsLate    bool
	IsDuplicate bool
	// LateBy is set on late packets and measured in frame steps.
	LateBy    int
}

// GeneratePackets synthesizes a deterministic RTP stream. The returned
// stream is in injection order, so loss/duplicate/late/wrap faults are
// already applied; the reorder buffer and timeline repair them.
func GeneratePackets(cfg Config) ([]Packet, error) {
	if cfg.FrameStep <= 0 {
		cfg.FrameStep = clock.Ticks(3600)
	}
	if cfg.DurationTicks <= 0 {
		return nil, fmt.Errorf("%w: non-positive duration", clock.ErrInvalidArgument)
	}
	if cfg.LossRate < 0 || cfg.LossRate >= 1 {
		return nil, fmt.Errorf("%w: loss rate out of range", clock.ErrInvalidArgument)
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	nFrames := int(cfg.DurationTicks / cfg.FrameStep)
	if nFrames > clock.MaxManifestSegments {
		return nil, fmt.Errorf("%w: too many frames", clock.ErrManifestTooLarge)
	}

	type frame struct {
		seq uint16
		ts  uint32
	}
	// Build the canonical frame table first; wrap is applied to the raw
	// timestamps so the timeline repair layer has something to fix.
	frames := make([]frame, nFrames)
	var ts uint32
	if cfg.Wrap {
		ts = uint32(clock.Max32-1) - uint32(nFrames/2)*uint32(cfg.FrameStep)
	}
	for i := 0; i < nFrames; i++ {
		frames[i] = frame{seq: uint16(i), ts: ts}
		ts += uint32(cfg.FrameStep)
	}

	var out []Packet
	emit := func(f frame, isRTX, isLate, isDup bool, lateBy int) {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: f.seq,
				Timestamp:      f.ts,
				SSRC:           4242,
				Marker:         true,
			},
			Payload: []byte{0x47, byte(i2b(f.seq))},
		}
		raw, err := pkt.Marshal()
		if err != nil {
			panic(err) // deterministic in-memory marshal cannot fail
		}
		out = append(out, Packet{
			Raw: raw, Seq: f.seq, TS: f.ts,
			IsRTX: isRTX, IsLate: isLate, IsDuplicate: isDup, LateBy: lateBy,
		})
	}

	for i, f := range frames {
		drop := rng.Float64() < cfg.LossRate
		dup := rng.Float64() < cfg.DuplicateRate
		if drop && i+1 < nFrames {
			// Simulate loss then RTX recovery two frames later: the frame
			// is skipped now and re-injected below as an RTX packet.
			emit(f, true, false, false, 0)
			// Mark by emitting out of band after the next frame instead.
			out = out[:len(out)-1]
			emit(frames[i+1], false, false, false, 0)
			emit(f, true, false, false, 0)
			i++
			continue
		}
		emit(f, false, false, false, 0)
		if dup {
			emit(f, false, false, true, 0)
		}
	}

	// Late out-of-order injection: take an early surviving frame and
	// re-append it after several later frames so it arrives late.
	if cfg.LateRate > 0 || cfg.LateOutside {
		lateIdx := nFrames / 4
		if lateIdx < nFrames {
			f := frames[lateIdx]
			emit(f, false, true, false, nFrames/8)
		}
	}
	return out, nil
}

func i2b(v uint16) int { return int(v & 0xff) }

// Segment is a generated fMP4 fragment plus its manifest metadata.
type Segment struct {
	TrackID  int
	Sequence int
	Start    clock.Ticks
	Duration clock.Ticks
	Data     []byte
	Truncated bool
}

// GenerateSegments builds one media segment per SegmentStep using the
// ingest box builder, so the generated bytes parse through the same
// validator the production pipeline uses.
func GenerateSegments(cfg Config, trackID int) ([]Segment, error) {
	if cfg.SegmentStep <= 0 {
		cfg.SegmentStep = clock.Ticks(2 * clock.Rate)
	}
	nSegs := int(cfg.DurationTicks / cfg.SegmentStep)
	if nSegs > clock.MaxManifestSegments {
		return nil, fmt.Errorf("%w: too many segments", clock.ErrManifestTooLarge)
	}
	out := make([]Segment, 0, nSegs)
	for i := 0; i < nSegs; i++ {
		b := &ingest.BoxBuilder{}
		b.Box("styp", []byte("msdhmsdh"))
		moof := &ingest.BoxBuilder{}
		fullBox(moof, "tfhd", 0, 0x020000, []byte{0, 0, 0, byte(trackID + 1)})
		trunPayload := []byte{
			0, 0, 0, 1, // one sample
			byte(cfg.SegmentStep >> 24), byte(cfg.SegmentStep >> 16),
			byte(cfg.SegmentStep >> 8), byte(cfg.SegmentStep),
			0, 0, 0, 16,
		}
		fullBox(moof, "trun", 0, 0x000300, trunPayload)
		b.Container("moof", moof)
		b.Box("mdat", make([]byte, 16))

		data := b.Bytes()
		truncated := false
		// Inject one truncated fragment at a deterministic position.
		if cfg.TruncateBox && i == nSegs/2 {
			data = b.Truncate(len(data) / 2)
			truncated = true
		}
		out = append(out, Segment{
			TrackID:   trackID,
			Sequence:  i,
			Start:     clock.Ticks(i) * cfg.SegmentStep,
			Duration:  cfg.SegmentStep,
			Data:      data,
			Truncated: truncated,
		})
	}
	return out, nil
}

// fullBox mirrors the test helper in ingest but stays in the fixtures
// package so generated samples need no test-only exports.
func fullBox(b *ingest.BoxBuilder, typ string, version byte, flags uint32, payload []byte) {
	vf := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	b.Box(typ, append(vf, payload...))
}

// HLSTemplate renders a minimal media playlist for the generated segs.
func HLSTemplate(seqs []int, targetDurSecs int) []byte {
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:"...)
	b = append(b, fmt.Sprintf("%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", targetDurSecs, seqs[0])...)
	for _, s := range seqs {
		b = append(b, "#EXTINF:0.04,\n"...)
		b = append(b, fmt.Sprintf("seg%d.m4s\n", s)...)
	}
	return b
}
