package fixture

import (
	"encoding/binary"

	"github.com/pion/rtp"

	"mediacheck/internal/metadata"
)

// buildMP4 assembles a minimal but structurally valid box hierarchy:
// ftyp / moov(mvhd, trak(mdia(mdhd, minf(stbl)))) / mdat. The
// truncated profile cuts the mdat box short to provoke a fault.
func buildMP4(cfg Config, video, audio []Sample) []byte {
	ftyp := box("ftyp", []byte("isommp41dash"))
	mvhd := box("mvhd", u32(cfg.VideoTS))
	mdhd := box("mdhd", append(u32(cfg.VideoTS), u32(cfg.AudioTS)...))
	stbl := box("stbl",
		box("stsd", []byte("vidaudt")),
		box("stsz", u32(uint32(len(video)+len(audio)))),
	)
	minf := box("minf", stbl)
	mdia := box("mdia", mdhd, minf)
	trak := box("trak", mdia)
	moov := container("moov", mvhd, trak)

	mdatPayload := make([]byte, 256)
	for i := range mdatPayload {
		mdatPayload[i] = byte(i % 251)
	}
	mdat := box("mdat", mdatPayload)
	if cfg.Profile == ProfileTruncated {
		// Keep the header but cut the payload before its declared end.
		mdat = mdat[:8+len(mdatPayload)/2]
	}

	out := append(ftyp, moov...)
	out = append(out, mdat...)
	return out
}

// buildRTP emits a deterministic per-40ms packet stream. The configured
// burst is split into a reorderable head and a truly-lost tail. The
// head is delivered late (after later packets) so the window can
// restore it in order; the tail never arrives and stays a real gap.
func buildRTP(cfg Config) ([]*rtp.Packet, Span, int) {
	const clockRate = 90000
	const stepMs = 40
	packets := []*rtp.Packet{}
	count := int(cfg.DurationMs / stepMs)
	burstStart := count / 2
	burstEnd := burstStart + cfg.LossBurst
	// Most of the burst is recoverable via in-window reordering; the
	// remaining tail is hard loss. A strictly normal profile keeps the
	// whole burst reorderable so healthy media exits cleanly.
	hardLoss := cfg.LossBurst / 12
	if cfg.Profile == ProfileNormal {
		hardLoss = 0
	}
	reorderable := cfg.LossBurst - hardLoss
	reorderEnd := burstStart + reorderable
	gap := Span{
		StartMs: int64(burstStart) * stepMs,
		EndMs:   int64(burstEnd) * stepMs,
	}

	type frame struct {
		seq uint16
		ts  uint32
		idx int
	}
	ordered := []frame{}
	var seq uint16 = 1000
	ts := uint32(cfg.Seed * 1000)
	for i := 0; i < count; i++ {
		ordered = append(ordered, frame{seq: seq, ts: ts, idx: i})
		seq++
		ts += uint32(clockRate * stepMs / 1000)
	}

	emit := func(f frame) {
		packets = append(packets, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: f.seq,
				Timestamp:      f.ts,
				SSRC:           0x5151aaaa,
			},
			Payload: []byte{byte(f.idx % 251)},
		})
	}

	// Phase 1: everything up to the burst, in order.
	for _, f := range ordered {
		if f.idx >= burstStart {
			break
		}
		emit(f)
	}
	// Phase 2: the packet that closes the burst, then the later stream.
	// The missing head is observed here as an in-window gap.
	for _, f := range ordered {
		if f.idx >= burstEnd {
			emit(f)
		}
	}
	// Phase 3: deliver the held reorderable head late, once each. These
	// fill the gap in order; the hard-loss tail never arrives, leaving a
	// genuine unrecovered hole before the committed frontier.
	for _, f := range ordered {
		if f.idx >= burstStart && f.idx < reorderEnd {
			emit(f)
		}
	}
	return packets, gap, reorderable
}

// buildEvents creates one of each supported carrier at fixed times so
// ordering and alignment are reproducible across runs.
func buildEvents(cfg Config) []metadata.Event {
	evs := []metadata.Event{}
	seq := 0
	add := func(k metadata.Kind, track string, start int64, payload string) {
		evs = append(evs, metadata.Event{
			Kind: k, Track: track, StartMs: start, EndMs: start + 200,
			Payload: payload, Seq: seq,
		})
		seq++
	}
	// Two events share timestamp 500 to exercise stable tie-breaking.
	add(metadata.KindChapter, videoTrack, 0, "chapter:open")
	add(metadata.KindEMSG, videoTrack, 500, "emsg:ad-start")
	add(metadata.KindID3, audioTrack, 500, "id3:beacon")
	add(metadata.KindSEI, videoTrack, 1000, "sei:frame-mark")
	add(metadata.KindSubtitle, videoTrack, 1500, "subtitle:cue-1")
	if cfg.Profile == ProfileBoundary {
		// An event beyond every track window: must be retained as orphan.
		add(metadata.KindID3, audioTrack, cfg.DurationMs+5000, "id3:orphan")
	}
	return evs
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}
