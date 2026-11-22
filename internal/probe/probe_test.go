package probe

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// rawBox builds a box with the given type and payload using a 32-bit size.
func rawBox(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	return append(out, payload...)
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func be64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func cat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// testInit builds an init segment with mvhd and one audio track at the given
// timescale. Track IDs and durations are deterministic.
func testInit(timescale uint32, trackID uint32) []byte {
	ftyp := rawBox("ftyp", cat([]byte("isom"), be32(512), []byte("isomiso6")))
	mvhd := rawBox("mvhd", cat(
		[]byte{0, 0, 0, 0},                    // version/flags
		be32(0), be32(0), be32(1000), be32(0), // creation, modification, timescale, duration
		be32(0x00010000), be16(0x0100), be16(0), // rate, volume, reserved
		be32(0), be32(0), // reserved
		make([]byte, 36), // matrix
		make([]byte, 24), // pre_defined
		be32(2),          // next_track_ID
	))
	mdhd := rawBox("mdhd", cat(
		[]byte{0, 0, 0, 0},
		be32(0), be32(0), be32(timescale), be32(0),
		be16(0x55c4), be16(0), // language "und", pre_defined
	))
	hdlr := rawBox("hdlr", cat(
		[]byte{0, 0, 0, 0}, be32(0), []byte("soun"), make([]byte, 12), []byte("Sound\x00"),
	))
	smhd := rawBox("smhd", cat([]byte{0, 0, 0, 0}, be16(0), be16(0)))
	mp4a := rawBox("mp4a", cat(
		make([]byte, 6), be16(1), // reserved, data_reference_index
		be16(0), be16(0), be16(0), be16(0), // version, reserved, channelcount, samplesize
		be16(0), be16(0), // pre_defined, reserved
		be32(timescale<<16), // sample rate fixed 16.16
	))
	stsd := rawBox("stsd", cat([]byte{0, 0, 0, 0}, be32(1), mp4a))
	stbl := rawBox("stbl", stsd)
	minf := rawBox("minf", cat(smhd, stbl))
	mdia := rawBox("mdia", cat(mdhd, hdlr, minf))
	tkhd := rawBox("tkhd", cat(
		[]byte{0, 0, 0, 7}, // version 0, flags enabled|in_movie|in_preview
		be32(0), be32(0), be32(trackID), be32(0), be32(0),
		make([]byte, 8), be16(0), be16(0), be16(0x0100), be16(0),
		make([]byte, 36), be32(0), be32(0),
	))
	trak := rawBox("trak", cat(tkhd, mdia))
	trex := rawBox("trex", cat([]byte{0, 0, 0, 0}, be32(trackID), be32(1), be32(0), be32(0), be32(0)))
	mvex := rawBox("mvex", trex)
	moov := rawBox("moov", cat(mvhd, trak, mvex))
	return cat(ftyp, moov)
}

type segOptions struct {
	sequence   uint32
	trackID    uint32
	baseTime   uint64
	sampleDur  uint32
	sampleSize uint32
	samples    int
	syncFirst  bool
}

// testMedia builds a single-fragment media segment.
func testMedia(o segOptions) []byte {
	styp := rawBox("styp", cat([]byte("msdh"), be32(0), []byte("msdhmsix")))
	mfhd := rawBox("mfhd", cat([]byte{0, 0, 0, 0}, be32(o.sequence)))
	tfhd := rawBox("tfhd", cat([]byte{0, 0x02, 0x00, 0x00}, be32(o.trackID))) // flags: default-base-is-moof
	tfdt := rawBox("tfdt", cat([]byte{1, 0, 0, 0}, be64(o.baseTime)))
	flags := make([]uint32, o.samples)
	for i := range flags {
		flags[i] = 0x01010000 // depends_on=1, non-sync
		if i == 0 && o.syncFirst {
			flags[i] = 0x02000000
		}
	}
	trunPayload := cat(
		[]byte{0, 0x00, 0x07, 0x05}, // version 0; flags: data-offset, first-sample-flags, duration, size, flags
		be32(uint32(o.samples)),
		be32(0),          // placeholder data offset
		be32(0x02000000), // first sample flags
	)
	for i := 0; i < o.samples; i++ {
		trunPayload = cat(trunPayload, be32(o.sampleDur), be32(o.sampleSize), be32(flags[i]))
	}
	trun := rawBox("trun", trunPayload)
	traf := rawBox("traf", cat(tfhd, tfdt, trun))
	moof := rawBox("moof", cat(mfhd, traf))
	mdatData := make([]byte, int(o.sampleSize)*o.samples)
	for i := range mdatData {
		mdatData[i] = byte(i % 251)
	}
	mdat := rawBox("mdat", mdatData)
	return cat(styp, moof, mdat)
}

func TestScanBoxesDetectsOverflow(t *testing.T) {
	data := rawBox("ftyp", []byte("isomxxxx"))
	// A box claiming far more bytes than remain.
	over := make([]byte, 8)
	binary.BigEndian.PutUint32(over[:4], 4096)
	copy(over[4:], "mdat")
	data = append(data, over...)
	boxes, faults := scanBoxes(bytes.NewReader(data), 0, int64(len(data)), 100)
	if len(boxes) != 2 || boxes[1].Complete {
		t.Fatalf("boxes = %+v", boxes)
	}
	if len(faults) != 1 || faults[0].Kind != "box_overflow" || faults[0].Loc.Offset != int64(len(data)-8) {
		t.Fatalf("faults = %+v", faults)
	}
}

func TestScanBoxesTruncatedTail(t *testing.T) {
	data := append(rawBox("ftyp", []byte("isom")), 0x00, 0x00, 0x01)
	_, faults := scanBoxes(bytes.NewReader(data), 0, int64(len(data)), 100)
	if len(faults) != 1 || faults[0].Kind != "truncated_tail" || faults[0].Loc.Offset != 12 {
		t.Fatalf("faults = %+v", faults)
	}
}

func TestParseInit(t *testing.T) {
	data := testInit(48000, 7)
	info, faults, err := ParseInit(bytes.NewReader(data), int64(len(data)), "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if faults.Len() != 0 {
		t.Fatalf("faults: %v", faults.Err())
	}
	if !info.HasFtyp || !info.HasMoov || info.MajorBrand != "isom" {
		t.Fatalf("info = %+v", info)
	}
	if len(info.Tracks) != 1 || info.Tracks[0].Timescale != 48000 || info.Tracks[0].TrackID != 7 {
		t.Fatalf("tracks = %+v", info.Tracks)
	}
	if info.Tracks[0].Handler != "soun" {
		t.Fatalf("handler = %q", info.Tracks[0].Handler)
	}
}

func TestParseMediaFragment(t *testing.T) {
	data := testMedia(segOptions{sequence: 3, trackID: 7, baseTime: 96000, sampleDur: 1024, sampleSize: 100, samples: 4, syncFirst: true})
	info, faults, err := ParseMedia(bytes.NewReader(data), int64(len(data)), "seg3.m4s", map[uint32]uint32{7: 48000})
	if err != nil {
		t.Fatal(err)
	}
	if faults.Len() != 0 {
		t.Fatalf("faults: %v", faults.Err())
	}
	if !info.HasMoof || !info.HasMdat || len(info.Fragments) != 1 {
		t.Fatalf("info = %+v", info)
	}
	frag := info.Fragments[0]
	if frag.Sequence != 3 || frag.TrackID != 7 || frag.BaseTime != 96000 {
		t.Fatalf("frag = %+v", frag)
	}
	if frag.Duration != 4096 || len(frag.Samples) != 4 {
		t.Fatalf("duration/samples = %d/%d", frag.Duration, len(frag.Samples))
	}
	if !frag.Samples[0].Sync || frag.Samples[1].Sync {
		t.Fatalf("sync flags = %+v", frag.Samples)
	}
	// 96000 ticks at 48 kHz = 2 s = 180000 at 90 kHz.
	if info.StartTicks != 180000 {
		t.Fatalf("start = %d", info.StartTicks)
	}
	// 4096 ticks at 48 kHz = 85.333 ms -> 7680 canonical ticks.
	if info.EndTicks-info.StartTicks != 7680 {
		t.Fatalf("duration ticks = %d", info.EndTicks-info.StartTicks)
	}
	if !info.Determined {
		t.Fatal("timeline should be determined")
	}
}

func TestParseMediaMissingMdat(t *testing.T) {
	data := testMedia(segOptions{sequence: 1, trackID: 7, baseTime: 0, sampleDur: 1024, sampleSize: 100, samples: 2})
	// Drop the whole mdat box (last 8+200 bytes).
	data = data[:len(data)-208]
	info, faults, err := ParseMedia(bytes.NewReader(data), int64(len(data)), "bad.m4s", map[uint32]uint32{7: 48000})
	if err != nil {
		t.Fatal(err)
	}
	if info.HasMdat {
		t.Fatal("mdat should be absent")
	}
	kinds := faults.ByKind()
	if kinds["missing_mdat"] != 1 {
		t.Fatalf("faults = %v", faults.All())
	}
	if len(info.Fragments) != 1 || info.Fragments[0].HasMdat {
		t.Fatalf("fragment = %+v", info.Fragments)
	}
}

func TestParseMediaTruncatedTail(t *testing.T) {
	data := testMedia(segOptions{sequence: 1, trackID: 7, baseTime: 0, sampleDur: 1024, sampleSize: 100, samples: 2})
	data = data[:len(data)-120] // cut inside mdat payload
	info, faults, err := ParseMedia(bytes.NewReader(data), int64(len(data)), "cut.m4s", map[uint32]uint32{7: 48000})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Truncated {
		t.Fatal("expected truncation flag")
	}
	if faults.ByKind()["box_overflow"] == 0 {
		t.Fatalf("expected overflow fault, got %v", faults.All())
	}
}

func TestParseMediaUndeterminedWithoutTimescale(t *testing.T) {
	data := testMedia(segOptions{sequence: 1, trackID: 7, baseTime: 0, sampleDur: 1024, sampleSize: 100, samples: 2})
	info, _, err := ParseMedia(bytes.NewReader(data), int64(len(data)), "noinit.m4s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Determined {
		t.Fatal("timeline must stay undetermined without a timescale")
	}
	if info.EndTicks != 0 || info.StartTicks != 0 {
		t.Fatalf("fabricated timeline: %+v", info)
	}
}

func TestCheckContinuity(t *testing.T) {
	mk := func(seq uint32, base uint64, dur uint32) Fragment {
		return Fragment{Sequence: seq, BaseTime: base, Timescale: 48000, Duration: dur * 4, Determined: true, MoofOffset: int64(seq) * 100}
	}
	ok := CheckContinuity([]Fragment{mk(1, 0, 1024), mk(2, 4096, 1024)}, "s")
	if len(ok) != 0 {
		t.Fatalf("unexpected faults: %+v", ok)
	}
	gap := CheckContinuity([]Fragment{mk(1, 0, 1024), mk(2, 8192, 1024)}, "s")
	if len(gap) != 1 || gap[0].Kind != "timestamp_discontinuity" || gap[0].Loc.Offset != 200 {
		t.Fatalf("gap faults: %+v", gap)
	}
}
