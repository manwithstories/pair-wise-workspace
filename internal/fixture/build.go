// Package fixture generates deterministic Matroska sample archives and RTP
// packet streams for the archivecheck demo.  Every byte derives from the seed;
// no external media is read.  Damage injection is explicit in Defect so the
// same generator produces the normal and corrupt samples.
package fixture

import (
	"fmt"
	"math/rand"
	"sort"

	"archivecheck/internal/clock"
	"archivecheck/internal/ebml"
)

// SegmentTimescale is the generated document's timestamp scale (ticks/s).
const SegmentTimescale = 1_000_000

// KeyframeEvery marks every Nth video frame as a keyframe.
const KeyframeEvery = 8

// TrackSpec describes one generated track.
type TrackSpec struct {
	Number      uint64
	Kind        ebml.TrackKind
	Codec       string
	Timescale   int64 // frame clock units per second
	FrameDur    int64 // frame duration in those units
	Scale       string
	PayloadSize int // approximate video payload bytes
}

// Defect selects damage the corrupt sample carries.  Zero values inject no
// timestamp damage; set HasRegress/HasDuplicate to enable it.
type Defect struct {
	TruncateAt         int
	HasRegress         bool
	TimestampRegressAt int
	HasDuplicate       bool
	DuplicateAt        int
	UnknownSizeSegment bool
	UnknownSizeCluster bool
}

// Frame is one generated block, kept for reference and quality checks.
type Frame struct {
	Track      uint64
	Ordinal    int
	Time       clock.Rational // intended media time
	Stored     clock.Rational // time written into the block (may regress)
	Offset     int64          // block element header offset
	PayloadOff int64
	PayloadLen int64
	Keyframe   bool
	PCM        []int16
	Raw        []byte
}

// Plan is the generated document plus its reference data.
type Plan struct {
	Data            []byte
	Header          ebml.Header
	Frames          []Frame
	TargetBps       int64
	DurationSeconds float64
	Truncated       bool
}

// PayloadBytes sums the media payload the archive is expected to carry.
func (p *Plan) PayloadBytes() int64 {
	var n int64
	for _, f := range p.Frames {
		if len(f.PCM) > 0 {
			n += int64(len(f.PCM)) * 2
		} else {
			n += int64(len(f.Raw))
		}
	}
	return n
}

type pending struct {
	spec    TrackSpec
	ordinal int
	time    clock.Rational // intended
	stored  clock.Rational // as written
	key     bool
	pcm     []int16
	raw     []byte
}

// Build generates the document.  frameCount is the number of frames on the
// longest track.
func Build(seed int64, tracks []TrackSpec, frameCount int, d Defect) (*Plan, error) {
	if len(tracks) == 0 {
		return nil, fmt.Errorf("fixture: no tracks")
	}
	if frameCount <= 0 {
		return nil, fmt.Errorf("fixture: frame count must be positive")
	}
	rng := rand.New(rand.NewSource(seed))
	plan := &Plan{Header: ebml.Header{TimestampScale: SegmentTimescale, Tracks: map[uint64]*ebml.Track{}}}

	for _, ts := range tracks {
		if ts.Timescale <= 0 || ts.FrameDur <= 0 {
			return nil, fmt.Errorf("fixture: track %d needs positive timescale and frame duration", ts.Number)
		}
		tr := &ebml.Track{
			Number: ts.Number, UID: ts.Number*1000 + 1, Kind: ts.Kind, CodecID: ts.Codec,
			TimestampScale: clock.MustNew(1, 1), Language: "eng",
			DefaultDur: uint64(1e9) * uint64(ts.FrameDur) / uint64(ts.Timescale),
		}
		if ts.Scale != "" {
			sc, err := clock.ParseDecimal(ts.Scale)
			if err != nil {
				return nil, err
			}
			tr.TimestampScale = sc
		}
		if ts.Kind == ebml.TrackVideo {
			tr.PixelWidth, tr.PixelHeight = 64, 36
		}
		if ts.Kind == ebml.TrackAudio {
			tr.Channels, tr.SampleRate = 1, uint64(ts.Timescale)
		}
		plan.Header.Tracks[ts.Number] = tr
		plan.Header.TrackOrder = append(plan.Header.TrackOrder, ts.Number)
	}

	var frames []pending
	for _, spec := range tracks {
		count := frameCount
		if spec.Kind == ebml.TrackSubtitle {
			count = frameCount / 4
			if count < 1 {
				count = 1
			}
		}
		gen := &RawAudio{SampleRate: uint32(spec.Timescale), Channels: 1, Freq: 440 + 37*float64(spec.Number)}
		scale := plan.Header.Tracks[spec.Number].TimestampScale
		for i := 0; i < count; i++ {
			ticks := int64(i) * spec.FrameDur
			t, err := clock.FromTicks(ticks, spec.Timescale)
			if err != nil {
				return nil, err
			}
			t = t.Mul(scale)
			stored := t
			if d.HasRegress && i == d.TimestampRegressAt && i > 0 {
				stored = t.Sub(clock.MustNew(1, 1000))
			}
			if d.HasDuplicate && i == d.DuplicateAt && i > 0 {
				prev, _ := clock.FromTicks(int64(i-1)*spec.FrameDur, spec.Timescale)
				stored = prev.Mul(scale)
			}
			p := pending{spec: spec, ordinal: i, time: t, stored: stored,
				key: spec.Kind == ebml.TrackVideo && i%KeyframeEvery == 0}
			switch spec.Kind {
			case ebml.TrackAudio:
				p.pcm = gen.Next(64)
			case ebml.TrackVideo:
				n := spec.PayloadSize / 2
				if n < 16 {
					n = 16
				}
				p.pcm = make([]int16, n)
				base := int16((i*13 + int(seed)) % 4096)
				for k := range p.pcm {
					p.pcm[k] = base + int16(k%17) + int16(rng.Intn(3))
				}
			default:
				p.raw = []byte(fmt.Sprintf("subtitle line %04d", i))
			}
			frames = append(frames, p)
		}
	}
	sort.SliceStable(frames, func(i, j int) bool {
		if c := frames[i].stored.Cmp(frames[j].stored); c != 0 {
			return c < 0
		}
		return frames[i].spec.Number < frames[j].spec.Number
	})

	// Encode header.
	doc := &ebml.Encoder{}
	doc.Master(ebml.IDEBML, func(e *ebml.Encoder) {
		e.Uint(ebml.IDEBMLVersion, 1)
		e.Uint(ebml.IDEBMLReadVersion, 1)
		e.Uint(ebml.IDEBMLMaxIDLength, 4)
		e.Uint(ebml.IDEBMLMaxSizeLength, 8)
		e.String(ebml.IDDocType, "matroska")
		e.Uint(ebml.IDDocTypeVersion, 4)
	})

	seg := &ebml.Encoder{}
	longest := tracks[0]
	durationMS := float64(frameCount) * float64(longest.FrameDur) / float64(longest.Timescale) * 1000
	plan.DurationSeconds = durationMS / 1000
	seg.Master(ebml.IDInfo, func(e *ebml.Encoder) {
		e.Uint(ebml.IDTimestampScale, SegmentTimescale)
		e.Float(ebml.IDDuration, durationMS)
		e.String(ebml.IDMuxingApp, "archivecheck-fixture")
		e.String(ebml.IDWritingApp, "archivecheck-fixture")
	})
	seg.Master(ebml.IDTracks, func(e *ebml.Encoder) {
		for _, spec := range tracks {
			spec := spec
			e.Master(ebml.IDTrackEntry, func(e *ebml.Encoder) {
				e.Uint(ebml.IDTrackNumber, spec.Number)
				e.Uint(ebml.IDTrackUID, spec.Number*1000+1)
				e.Uint(ebml.IDTrackType, uint64(spec.Kind))
				e.String(ebml.IDCodecID, spec.Codec)
				if spec.Scale != "" {
					sc, _ := clock.ParseDecimal(spec.Scale)
					e.Float(ebml.IDTrackTimestampScale, sc.Float64())
				}
				e.Uint(ebml.IDDefaultDuration, uint64(1e9)*uint64(spec.FrameDur)/uint64(spec.Timescale))
				switch spec.Kind {
				case ebml.TrackVideo:
					e.Master(ebml.IDVideo, func(e *ebml.Encoder) {
						e.Uint(ebml.IDPixelWidth, 64)
						e.Uint(ebml.IDPixelHeight, 36)
					})
				case ebml.TrackAudio:
					e.Master(ebml.IDAudio, func(e *ebml.Encoder) {
						e.Uint(ebml.IDChannels, 1)
						e.Float(ebml.IDSamplingFrequency, float64(spec.Timescale))
					})
				}
			})
		}
	})

	// Group into 500ms clusters; record the absolute offset where each block's
	// header will land so the index and manifest can reference real bytes.
	clusters := &ebml.Encoder{}
	current := &ebml.Encoder{}
	var currentTick int64
	haveCluster := false
	flush := func() {
		if !haveCluster {
			return
		}
		body := &ebml.Encoder{}
		body.Uint(ebml.IDClusterTimestamp, uint64(currentTick))
		body.RawBytes(current.Bytes())
		if d.UnknownSizeCluster {
			clusters.UnknownHeader(ebml.IDCluster, 8)
			clusters.RawBytes(body.Bytes())
		} else {
			clusters.MasterBytes(ebml.IDCluster, body.Bytes())
		}
		current = &ebml.Encoder{}
		haveCluster = false
	}
	// Segment payload begins after the segment header; compute it after
	// encoding the pre-cluster portion.
	segPrefix := &ebml.Encoder{}
	segPrefix.RawBytes(seg.Bytes())
	var segmentHeaderLen int64
	if d.UnknownSizeSegment {
		segmentHeaderLen = int64(4 + 8)
	} else {
		segmentHeaderLen = 4 + int64(len(ebml.EncodeSize(uint64(seg.Len()+clusters.Len()), 0)))
	}
	segHead := &ebml.Encoder{}
	segHead.ID(ebml.IDSegment)
	if d.UnknownSizeSegment {
		segHead.RawBytes(ebml.EncodeUnknownSize(8))
	} else {
		// Size is patched later; reserve a 4-byte known size now by encoding a
		// placeholder and rebuilding at the end.
	}

	// Encode clusters with offset tracking.  Since cluster bytes are appended
	// to segHead's body, offsets depend on the segment header length and the
	// pre-cluster segment body, both known before the first block.
	bodyPrefix := segPrefix.Bytes()
	_ = bodyPrefix
	_ = segmentHeaderLen
	_ = segHead
	for i := range frames {
		f := &frames[i]
		tick := segmentTick(f.stored)
		window := tick / 500
		if !haveCluster {
			currentTick = window * 500
			haveCluster = true
		} else if window != currentTick/500 {
			flush()
			currentTick = window * 500
			haveCluster = true
		}
		hdr := ebml.EncodeBlockHeader(f.spec.Number, int16(tick-currentTick), 1, "", nil)
		var payload []byte
		if f.spec.Kind == ebml.TrackSubtitle {
			payload = f.raw
		} else {
			payload = EncodePCM(f.pcm)
		}
		blockEl := &ebml.Encoder{}
		blockEl.Binary(ebml.IDSimpleBlock, append(hdr, payload...))
		current.RawBytes(blockEl.Bytes())
	}
	flush()

	// Assemble the segment now that its total size is known.
	segOut := &ebml.Encoder{}
	segOut.ID(ebml.IDSegment)
	body := append(append([]byte{}, seg.Bytes()...), clusters.Bytes()...)
	if d.UnknownSizeSegment {
		segOut.RawBytes(ebml.EncodeUnknownSize(8))
	} else {
		segOut.RawBytes(ebml.EncodeSize(uint64(len(body)), 0))
	}
	segOut.RawBytes(body)
	doc.RawBytes(segOut.Bytes())
	full := doc.Bytes()

	// Locate each SimpleBlock by re-walking the encoded document.  This mirrors
	// what downstream parsers will see, so offsets are exact by construction.
	offsets, err := locateBlocks(full)
	if err != nil {
		return nil, err
	}
	if len(offsets) != len(frames) {
		return nil, fmt.Errorf("fixture: located %d blocks for %d frames", len(offsets), len(frames))
	}
	for i := range frames {
		f := &frames[i]
		loc := offsets[i]
		plan.Frames = append(plan.Frames, Frame{
			Track: f.spec.Number, Ordinal: f.ordinal, Time: f.time, Stored: f.stored,
			Offset: loc.header, PayloadOff: loc.payload, PayloadLen: loc.payloadLen,
			Keyframe: f.key, PCM: f.pcm, Raw: f.raw,
		})
	}
	plan.TargetBps = estimateBps(plan.Frames, plan.DurationSeconds)
	if d.TruncateAt > 0 {
		if d.TruncateAt >= len(full) {
			return nil, fmt.Errorf("fixture: truncation %d beyond file %d", d.TruncateAt, len(full))
		}
		full = full[:d.TruncateAt]
		plan.Truncated = true
	}
	plan.Data = full
	return plan, nil
}

type blockLoc struct {
	header     int64
	payload    int64
	payloadLen int64
	keyframe   bool
}

// locateBlocks walks the encoded bytes and records every SimpleBlock.
func locateBlocks(data []byte) ([]blockLoc, error) {
	r := ebml.NewReader(byteReaderAt(data), int64(len(data)), ebml.Options{})
	var out []blockLoc
	err := r.Walk(func(ph ebml.Phase, el ebml.Element) error {
		if ph != ebml.PhaseLeaf || el.ID != ebml.IDSimpleBlock {
			return nil
		}
		payload, _ := r.ReadPayload(el, 8)
		key := false
		if len(payload) >= 4 {
			trackWidth := 1
			for trackWidth < 8 && payload[trackWidth-1]&(0x80>>uint(trackWidth-1)) == 0 {
				trackWidth++
			}
			if 2+trackWidth < len(payload) {
				key = payload[2+trackWidth]&0x80 != 0
			}
		}
		out = append(out, blockLoc{header: el.HeaderOffset, payload: el.PayloadOffset, payloadLen: el.PayloadLen(), keyframe: key})
		return nil
	})
	return out, err
}

type byteReaderAt []byte

func (b byteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, errEOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, errEOF
	}
	return n, nil
}

var errEOF = eofSentinel{}

type eofSentinel struct{}

func (eofSentinel) Error() string { return "EOF" }

func segmentTick(t clock.Rational) int64 {
	return int64(t.Float64()*float64(SegmentTimescale) + 0.5)
}

func estimateBps(frames []Frame, seconds float64) int64 {
	var n int64
	for _, f := range frames {
		n += f.PayloadLen
	}
	if seconds <= 0 {
		return 0
	}
	return int64(float64(n)*8/seconds + 0.5)
}
