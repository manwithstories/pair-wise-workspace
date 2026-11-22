package fixture

import (
	"mediaslice/media"
)

// writeVideoTrak writes a video track: tkhd, edts/elst (non-zero media start),
// mdia(mdhd,hdlr,minf/stbl). One chunk holds all samples (stsc single entry),
// stco points at the mdat payload start.
func writeVideoTrak(bw *BoxWriter, p *Plan) error {
	must(bw.Start("trak"))
	writeTkhd(bw, 1, uint32(p.Spec.DurationTicks), 0)

	// edts/elst: single full-length edit starting at a non-zero media time.
	must(bw.Start("edts"))
	must(bw.Start("elst"))
	must(bw.FullBox(0, 0))
	buf := U32(nil, 1) // entry count
	buf = U32(buf, uint32(p.Spec.DurationTicks))
	buf = append(buf, u32i(int64(p.Spec.EditStartTicks))...) // media_time v0
	buf = append(buf, 0x00, 0x01, 0x00, 0x00)                 // rate 1.0
	_, _ = bw.Write(buf)
	must(bw.End())
	must(bw.End())

	must(bw.Start("mdia"))
	writeMdhd(bw, media.ClockRate, uint32(media.Ticks(len(p.Video))*3600+0))
	writeHdlr(bw, "vide", "VideoHandler")
	must(bw.Start("minf"))
	must(bw.Start("stbl"))
	writeVideoStbl(bw, p)
	must(bw.End()) // stbl
	must(bw.End()) // minf
	must(bw.End()) // mdia
	return bw.End() // trak
}

func writeAudioTrak(bw *BoxWriter, p *Plan) error {
	must(bw.Start("trak"))
	writeTkhd(bw, 2, 0, 0x0100)

	aDurRaw := uint32(0)
	if len(p.Audio) > 0 {
		aDurRaw = p.Audio[len(p.Audio)-1].StartRaw + p.Audio[len(p.Audio)-1].Duration
	}
	must(bw.Start("mdia"))
	writeMdhd(bw, p.Spec.AudioRate, aDurRaw)
	writeHdlr(bw, "soun", "AudioHandler")
	must(bw.Start("minf"))
	must(bw.Start("stbl"))
	writeAudioStbl(bw, p)
	must(bw.End())
	must(bw.End())
	must(bw.End())
	return bw.End()
}

func writeTkhd(bw *BoxWriter, id, duration uint32, volume uint16) {
	must(bw.Start("tkhd"))
	must(bw.FullBox(0, 3)) // track_enabled + in_movie
	buf := U32(nil, 0)
	buf = U32(buf, 0)
	buf = U32(buf, id)
	buf = U32(buf, 0) // reserved
	buf = U32(buf, duration)
	buf = append(buf, make([]byte, 8)...) // reserved
	buf = append(buf, 0, 0)               // layer
	buf = append(buf, 0, 0)               // alt group
	buf = append(buf, byte(volume>>8), byte(volume))
	buf = append(buf, 0, 0) // reserved
	buf = append(buf, identityMatrix()...)
	buf = U32(buf, 320<<16) // width
	buf = U32(buf, 240<<16) // height
	_, _ = bw.Write(buf)
	must(bw.End())
}

func writeMdhd(bw *BoxWriter, timescale, duration uint32) {
	must(bw.Start("mdhd"))
	must(bw.FullBox(0, 0))
	buf := U32(nil, 0)
	buf = U32(buf, 0)
	buf = U32(buf, timescale)
	buf = U32(buf, duration)
	buf = append(buf, 0x55, 0xc4, 0x00, 0x00) // language 'und' + pre_defined
	_, _ = bw.Write(buf)
	must(bw.End())
}

func writeHdlr(bw *BoxWriter, htype, name string) {
	must(bw.Start("hdlr"))
	must(bw.FullBox(0, 0))
	buf := U32(nil, 0) // pre_defined
	buf = append(buf, []byte(htype)...)
	buf = append(buf, make([]byte, 12)...) // reserved
	buf = append(buf, []byte(name)...)
	buf = append(buf, 0)
	_, _ = bw.Write(buf)
	must(bw.End())
}

func u32i(v int64) []byte {
	if v < 0 {
		return U32(nil, uint32(int32(v)))
	}
	return U32(nil, uint32(v))
}

// writeVideoStbl writes stsd(empty)/stts/stss/stsc/stsz/stco against the
// precomputed mdat layout. mdat begins after ftyp+moov; the offsets passed
// here are patched via mdatBase which the caller knows after serialization.
func writeVideoStbl(bw *BoxWriter, p *Plan) {
	writeEmptyStsd(bw, "vide")

	must(bw.Start("stts"))
	must(bw.FullBox(0, 0))
	entries := compressStts(p.Video)
	buf := U32(nil, uint32(len(entries)))
	for _, e := range entries {
		buf = U32(buf, e.count)
		buf = U32(buf, e.delta)
	}
	_, _ = bw.Write(buf)
	must(bw.End())

	must(bw.Start("stss"))
	must(bw.FullBox(0, 0))
	keys := keyframeNumbers(p)
	buf = U32(nil, uint32(len(keys)))
	for _, k := range keys {
		buf = U32(buf, k)
	}
	_, _ = bw.Write(buf)
	must(bw.End())

	writeStsc(bw, 1, uint32(len(p.Video)))
	writeStsz(bw, uint32(p.Spec.VideoSampleSize), len(p.Video))
	writeStco(bw, p.videoChunkOffset)
}

func writeAudioStbl(bw *BoxWriter, p *Plan) {
	writeEmptyStsd(bw, "soun")
	must(bw.Start("stts"))
	must(bw.FullBox(0, 0))
	buf := U32(nil, 1)
	buf = U32(buf, uint32(len(p.Audio)))
	buf = U32(buf, 1) // 1 timescale unit per sample
	_, _ = bw.Write(buf)
	must(bw.End())
	writeStsc(bw, 1, uint32(len(p.Audio)))
	writeStsz(bw, uint32(p.Spec.AudioSampleSize), len(p.Audio))
	writeStco(bw, p.audioChunkOffset)
}

func writeEmptyStsd(bw *BoxWriter, htype string) {
	must(bw.Start("stsd"))
	must(bw.FullBox(0, 0))
	_, _ = bw.Write(U32(nil, 0)) // zero sample entries
	_ = htype
}

type sttsEntry struct{ count, delta uint32 }

func compressStts(vs []VideoSample) []sttsEntry {
	var out []sttsEntry
	for _, v := range vs {
		d := uint32(v.Duration)
		if n := len(out); n > 0 && out[n-1].delta == d {
			out[n-1].count++
			continue
		}
		out = append(out, sttsEntry{count: 1, delta: d})
	}
	return out
}

func keyframeNumbers(p *Plan) []uint32 {
	var keys []uint32
	for _, v := range p.Video {
		if v.IsKeyframe {
			keys = append(keys, uint32(v.Index+1))
		}
	}
	return keys
}

func writeStsc(bw *BoxWriter, chunks, samples uint32) {
	must(bw.Start("stsc"))
	must(bw.FullBox(0, 0))
	buf := U32(nil, 1) // one run
	buf = U32(buf, 1) // first_chunk
	buf = U32(buf, samples)
	buf = U32(buf, 1) // sample description index
	_ = chunks
	_, _ = bw.Write(buf)
	must(bw.End())
}

func writeStsz(bw *BoxWriter, fixed uint32, n int) {
	must(bw.Start("stsz"))
	must(bw.FullBox(0, 0))
	buf := U32(nil, fixed) // uniform sample size
	buf = U32(buf, uint32(n))
	_, _ = bw.Write(buf)
	must(bw.End())
}

func writeStco(bw *BoxWriter, offset uint32) {
	must(bw.Start("stco"))
	must(bw.FullBox(0, 0))
	buf := U32(nil, 1)
	buf = U32(buf, offset)
	_, _ = bw.Write(buf)
	must(bw.End())
}
