package mp4

import (
	"encoding/binary"
)

// FixtureSpec describes a deterministic synthetic ISO BMFF byte stream.
// Everything is generated in-process; no external files are referenced.
type FixtureSpec struct {
	Tracks    int
	Frames    int // samples per track
	Timescale int64
	// VFRDurations, when non-empty, is repeated cyclically as per-sample
	// timescale deltas, producing a variable frame rate track. When nil a
	// constant delta is used.
	VFRDurations []uint32
	// KeyEvery marks every Nth sample a keyframe (stss). Zero means no stss
	// (audio-style, every sample is a random access point).
	KeyEvery int
	// CorruptAtFrame truncates the mdat payload starting at this sample's
	// byte offset. Corrupt must be set explicitly; an unset spec is intact.
	CorruptAtFrame int
	// Corrupt enables the truncation described by CorruptAtFrame.
	Corrupt bool
	// TruncateHeader, when true, cuts the stream in the middle of a box
	// header instead of the mdat body.
	TruncateHeader bool
}

// box wraps payload with an 8-byte header (32-bit size).
func box(t BoxType, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], t[:])
	copy(out[8:], payload)
	return out
}

// u32appender builds payloads word by word.
type u32appender struct{ b []byte }

func (a *u32appender) u32(v uint32) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, v)
	a.b = append(a.b, buf...)
}

func (a *u32appender) u64(v uint64) {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	a.b = append(a.b, buf...)
}

// trackLayout records the final mdat byte offset of every frame in a track.
type trackLayout struct {
	frameOffsets []int64
}

// BuildFixture renders the spec into a self-contained byte stream. The layout
// is ftyp, moov(trak...) then mdat. Sample offsets point into mdat.
func BuildFixture(spec FixtureSpec) []byte {
	var out []byte
	// ftyp
	out = append(out, box(BoxFtyp, []byte("isom\x00\x00\x02\x00isom"))...)

	// Per-track sample sizes (constant 16 bytes).
	const sampleSize = int64(16)

	var traks [][]byte
	var layouts []trackLayout
	for t := 0; t < spec.Tracks; t++ {
		var lay trackLayout
		// mdhd version-0 layout: ver/flags(4), creation u32, modification
		// u32, timescale u32 at offset 12, duration u32, rate/volume/reserved.
		mdhdPayload := make([]byte, 32)
		binary.BigEndian.PutUint32(mdhdPayload[12:16], uint32(spec.Timescale))
		mdhd := box(BoxMdhd, mdhdPayload)

		// stts
		stts := &u32appender{}
		stts.u32(0) // version+flags
		if len(spec.VFRDurations) > 0 {
			// One entry per sample with count=1 so the cyclic durations
			// alternate exactly as specified (true variable frame rate).
			stts.u32(uint32(spec.Frames))
			for f := 0; f < spec.Frames; f++ {
				d := spec.VFRDurations[f%len(spec.VFRDurations)]
				stts.u32(1)
				stts.u32(d)
			}
		} else {
			stts.u32(1)
			stts.u32(uint32(spec.Frames))
			stts.u32(uint32(spec.Timescale / 30)) // 30 fps default
		}
		sttsBox := box(BoxStts, stts.b)

		// stss
		var stssBox []byte
		if spec.KeyEvery > 0 {
			stss := &u32appender{}
			stss.u32(0)
			var keys []uint32
			for f := 0; f < spec.Frames; f++ {
				if f%spec.KeyEvery == 0 {
					keys = append(keys, uint32(f+1))
				}
			}
			stss.u32(uint32(len(keys)))
			for _, k := range keys {
				stss.u32(k)
			}
			stssBox = box(BoxStss, stss.b)
		}

		// stsz (uniform size)
		stsz := &u32appender{}
		stsz.u32(0)
		stsz.u32(uint32(sampleSize))
		stsz.u32(uint32(spec.Frames))
		stszBox := box(BoxStsz, stsz.b)

		// stco placeholder: offsets patched once mdat position is known.
		stco := &u32appender{}
		stco.u32(0)
		stco.u32(uint32(spec.Frames))
		for i := 0; i < spec.Frames; i++ {
			stco.u32(0)
		}
		stcoBox := box(BoxStco, stco.b)
		lay.frameOffsets = make([]int64, spec.Frames)

		stbl := box(BoxStbl, concat(sttsBox, stssBox, stszBox, stcoBox))
		minf := box(BoxMinf, stbl)
		mdia := box(BoxMdia, concat(mdhd, minf))
		trak := box(BoxTrak, mdia)
		traks = append(traks, trak)
		layouts = append(layouts, lay)
	}
	moovPayload := concat(traks...)
	moov := box(BoxMoov, moovPayload)
	out = append(out, moov...)

	// mdat: one contiguous payload, frames laid out track by track.
	mdatPayloadLen := int64(spec.Tracks) * int64(spec.Frames) * sampleSize
	mdatTotal := 8 + mdatPayloadLen
	mdat := make([]byte, mdatTotal)
	binary.BigEndian.PutUint32(mdat[0:4], uint32(mdatTotal))
	copy(mdat[4:8], BoxMdat[:])
	mdatStartOff := int64(len(out)) + 8 // 8-byte mdat header
	cursor := mdatStartOff
	for t := 0; t < spec.Tracks; t++ {
		for f := 0; f < spec.Frames; f++ {
			layouts[t].frameOffsets[f] = cursor
			cursor += sampleSize
		}
	}
	// Patch stco entries by rewriting each trak (sizes stay identical).
	out = patchStco(out, layouts)
	out = append(out, mdat...)

	if spec.Corrupt {
		// Truncate at the first corrupted frame of track 0.
		cut := layouts[0].frameOffsets[spec.CorruptAtFrame]
		if spec.TruncateHeader {
			cut -= 4 // land inside the mdat header for header-truncation tests
		}
		if cut < int64(len(out)) {
			out = out[:cut]
		}
	}
	return out
}

// patchStco rewrites the zero placeholder offsets inside every stco box.
func patchStco(data []byte, layouts []trackLayout) []byte {
	roots, err := ScanBoxes(data, DefaultLimits())
	if err != nil {
		return data // fixture bug would surface in tests; leave unchanged
	}
	stcos := FindAll(roots, BoxStco)
	for t, stco := range stcos {
		if t >= len(layouts) {
			break
		}
		off := stco.Payload + 8 // skip version/flags + entry count
		for _, frameOff := range layouts[t].frameOffsets {
			binary.BigEndian.PutUint32(data[off:off+4], uint32(frameOff))
			off += 4
		}
	}
	return data
}

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
