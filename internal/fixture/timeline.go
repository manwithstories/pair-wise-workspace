// Package fixture generates every sample the pipeline consumes: the
// self-describing manifest, the MP4/H.264/AAC media files and the four
// damage cases. All randomness comes from the seeded randx source, so a
// given seed reproduces byte-identical packet tables, anchors and frames.
package fixture

import (
	"bq/internal/manifest"
	"bq/internal/randx"
)

// Options controls one fixture generation run.
type Options struct {
	Seed         uint64
	DurationS    int64
	LossRate     float64
	InjectWrap   bool
	InjectDup    bool
	InjectTrunc  bool
	AnchorConflicts int
	// PacketCadenceS is the logical packet spacing in seconds.
	PacketCadenceS int64
	// GroupSize is media packets per XOR group (plus one parity packet).
	GroupSize int64
}

// DefaultOptions returns the broadcast-demo defaults (30 min, 2% loss).
func DefaultOptions(seed uint64) Options {
	return Options{
		Seed: seed, DurationS: 1800, LossRate: 0.02,
		InjectWrap: true, PacketCadenceS: 1, GroupSize: 5,
	}
}

// streamBlueprint fixes the four tracks and their supplier timebases.
type streamBlueprint struct {
	id                int
	kind              string
	codec             string
	width, height     int
	timebaseNum, den  int64
	startPTS          int64
	wrapPeriod        int64
	keyframeEvery     int
	driftOffsetS      float64
	driftSlopeS       float64
	driftSegmentS     int64
	editStartS, editEndS int64
}

func blueprints() []streamBlueprint {
	return []streamBlueprint{
		{id: 0, kind: manifest.KindVideo, codec: "h264", width: 96, height: 54,
			timebaseNum: 90000, den: 1, startPTS: 90000, wrapPeriod: 600 * 90000,
			keyframeEvery: 30, driftOffsetS: 0.004, driftSlopeS: 0.001, driftSegmentS: 300,
			editStartS: 30, editEndS: 1770},
		{id: 1, kind: manifest.KindVideo, codec: "h264", width: 96, height: 54,
			timebaseNum: 15360, den: 1, startPTS: 15360, wrapPeriod: 480 * 15360,
			keyframeEvery: 30, driftOffsetS: -0.003, driftSlopeS: -0.0008, driftSegmentS: 300,
			editStartS: 30, editEndS: 1770},
		{id: 2, kind: manifest.KindVideo, codec: "h264", width: 96, height: 54,
			timebaseNum: 12000, den: 1, startPTS: 12000, wrapPeriod: 720 * 12000,
			keyframeEvery: 30, driftOffsetS: 0.002, driftSlopeS: 0.0006, driftSegmentS: 300,
			editStartS: 30, editEndS: 1770},
		{id: 3, kind: manifest.KindAudio, codec: "aac", width: 0, height: 0,
			timebaseNum: 48000, den: 1, startPTS: 48000, wrapPeriod: 900 * 48000,
			keyframeEvery: 1, driftOffsetS: 0.001, driftSlopeS: 0.0002, driftSegmentS: 300},
	}
}

// Build constructs the in-memory manifest (no files yet). The packet table,
// anchors and injection plan are fully deterministic from opts.Seed.
func Build(opts Options) *manifest.Manifest {
	rng := randx.NewPCG64(opts.Seed)
	bps := blueprints()
	m := &manifest.Manifest{
		Version:     "bq/1",
		Seed:        opts.Seed,
		DurationS:   opts.DurationS,
		MasterHz:    90000,
		GeneratedAt: "deterministic",
	}

	for _, bp := range bps {
		wrap := int64(0)
		if opts.InjectWrap {
			wrap = bp.wrapPeriod
		}
		s := manifest.Stream{
			ID: bp.id, Kind: bp.kind, Codec: bp.codec, Container: manifest.ContainerMP4,
			File: fileName(bp), Width: bp.width, Height: bp.height,
			TimebaseNum: bp.timebaseNum, TimebaseDen: bp.den, StartPTS: bp.startPTS,
			WrapPeriod: wrap, KeyframeEvery: bp.keyframeEvery,
			Drift: manifest.DriftSpec{
				OffsetS: bp.driftOffsetS, SlopeS: bp.driftSlopeS,
				SegmentS: bp.driftSegmentS,
			},
		}
		if bp.kind == manifest.KindVideo {
			s.EditList = []manifest.EditRange{
				{StartPTS: bp.editStartS * bp.timebaseNum, EndPTS: bp.editEndS * bp.timebaseNum},
			}
		}
		m.Streams = append(m.Streams, s)
		m.Anchors = append(m.Anchors, buildAnchors(bp, opts)...)
		m.Packets = append(m.Packets, buildPackets(bp, opts, rng)...)
	}

	planInjection(m, opts)
	return m
}

func fileName(bp streamBlueprint) string {
	if bp.kind == manifest.KindAudio {
		return "audio.m4a"
	}
	return "video" + string(rune('0'+bp.id)) + ".mp4"
}

// driftSeconds returns the modelled supplier offset at a given second.
func driftSeconds(bp streamBlueprint, atS int64) float64 {
	seg := atS / bp.driftSegmentS
	return bp.driftOffsetS + bp.driftSlopeS*float64(seg)
}

// sourcePTSAt maps a master second to the raw (pre-wrap) source PTS,
// incorporating start PTS and the piecewise drift model.
func sourcePTSAt(bp streamBlueprint, atS int64) int64 {
	drift := driftSeconds(bp, atS)
	driftTicks := int64(drift * float64(bp.timebaseNum))
	return bp.startPTS + atS*bp.timebaseNum + driftTicks
}

func buildAnchors(bp streamBlueprint, opts Options) []manifest.Anchor {
	var out []manifest.Anchor
	const anchorEveryS = 300
	for at := int64(0); at <= opts.DurationS; at += anchorEveryS {
		master := at * 90000
		out = append(out, manifest.Anchor{
			StreamID: bp.id, AtSecond: at,
			SourcePTS:   sourcePTSAt(bp, at),
			MasterTicks: master,
		})
	}
	return out
}

func buildPackets(bp streamBlueprint, opts Options, rng *randx.PCG64) []manifest.Packet {
	var out []manifest.Packet
	step := opts.PacketCadenceS
	if step <= 0 {
		step = 1
	}
	groupSize := opts.GroupSize
	if groupSize < 2 {
		groupSize = 5
	}
	var seq int64
	var offset int64 = 64 // leave a deterministic header region before data
	// Group ids must be globally unique: encode the stream id in the high
	// bits so transport's per-stream group maps never collide.
	groupBase := int64(bp.id) << 40
	for at := int64(0); at < opts.DurationS; at += step {
		localGroup := seq / (groupSize - 1)
		group := groupBase + localGroup
		pts := sourcePTSAt(bp, at)
		// VFR: jitter each frame duration deterministically within +/-8%.
		jitter := 1.0 + (float64(rng.Intn(17)-8))/100.0
		duration := int64(jitter * float64(step*bp.timebaseNum))
		keyframe := bp.kind == manifest.KindVideo && seq%int64(bp.keyframeEvery) == 0
		mp := manifest.Packet{
			StreamID: bp.id, Seq: seq, Kind: manifest.KindMedia, GroupID: group,
			SourceOffset: offset, RawPTS: pts, Keyframe: keyframe,
			DurationTick: duration, PayloadLen: 1024,
		}
		mp.XOR = mediaByte(mp)
		out = append(out, mp)
		offset += int64(mp.PayloadLen) + 16
		seq++

		// Emit one parity packet at the end of each XOR group.
		if seq%(groupSize-1) == 0 {
			groupMedia := filterGroup(out, bp.id, group)
			parity := manifest.Packet{
				StreamID: bp.id, Seq: -1, Kind: manifest.KindParity, GroupID: group,
				SourceOffset: offset, PayloadLen: 16,
			}
			parity.XOR = groupXOR(groupMedia)
			out = append(out, parity)
			offset += int64(parity.PayloadLen) + 16
		}
	}
	return out
}

func filterGroup(pkts []manifest.Packet, streamID int, group int64) []manifest.Packet {
	var out []manifest.Packet
	for _, p := range pkts {
		if p.StreamID == streamID && p.GroupID == group && p.Kind == manifest.KindMedia {
			out = append(out, p)
		}
	}
	return out
}

// mediaByte is the deterministic content byte shared with transport.
func mediaByte(p manifest.Packet) byte {
	h := uint64(p.StreamID)*1_000_003 + uint64(p.GroupID)*9_173 + uint64(p.Seq)*131 + 7
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	return byte(h)
}

func groupXOR(media []manifest.Packet) byte {
	var xor byte
	for _, p := range media {
		xor ^= p.XOR
	}
	return xor
}
