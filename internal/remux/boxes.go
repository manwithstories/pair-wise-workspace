package remux

import (
	"encoding/binary"

	"aircheck/internal/clock"
	"aircheck/internal/scan"
)

// buildMoov builds the moov box for the retained packets. Track durations
// are rescaled to the movie timescale (unified base).
func buildMoov(tracks []*trackOut) []byte {
	var traks [][]byte
	var movieDur uint32
	for _, tr := range tracks {
		traks = append(traks, box("trak", buildTrak(tr)))
		if d := tr.movieDuration(); d > movieDur {
			movieDur = d
		}
	}

	mvhdPayload := make([]byte, 100)
	be32(mvhdPayload[12:16], clock.Base) // timescale
	be32(mvhdPayload[16:20], movieDur)   // duration
	be32(mvhdPayload[20:24], 0x00010000) // rate 1.0
	binary.BigEndian.PutUint16(mvhdPayload[24:26], 0x0100) // volume 1.0
	writeMatrix(mvhdPayload[36:72])
	be32(mvhdPayload[96:100], uint32(len(tracks)+1)) // next track id
	mvhd := fullbox("mvhd", 0, 0, mvhdPayload)

	parts := append([][]byte{mvhd}, traks...)
	return box("moov", concat(parts))
}

func (tr *trackOut) scale() uint32 {
	if tr.timescale == 0 {
		return clock.Base
	}
	return tr.timescale
}

// movieDuration is the track duration rescaled to the movie timescale.
func (tr *trackOut) movieDuration() uint32 {
	d, err := clock.Rescale(int64(trackDuration(tr.pkts)), tr.scale(), clock.Base)
	if err != nil || d < 0 {
		return 0
	}
	return uint32(d)
}

func trackDuration(pkts []scan.Packet) uint32 {
	if len(pkts) == 0 {
		return 0
	}
	last := pkts[len(pkts)-1].DTS
	first := pkts[0].DTS
	dur := last - first
	if len(pkts) > 1 {
		dur += (last - first) / int64(len(pkts)-1) // one average sample duration
	}
	if dur < 0 {
		dur = 0
	}
	return uint32(dur)
}

func buildTrak(tr *trackOut) []byte {
	tkhdPayload := make([]byte, 84)
	be32(tkhdPayload[8:12], tr.id)           // track id
	be32(tkhdPayload[20:24], tr.movieDuration()) // duration (movie scale)
	writeMatrix(tkhdPayload[40:76])
	tkhd := fullbox("tkhd", 0, 0x3, tkhdPayload)

	mdhdPayload := make([]byte, 20)
	be32(mdhdPayload[8:12], tr.scale())          // timescale
	be32(mdhdPayload[12:16], trackDuration(tr.pkts)) // duration
	mdhd := fullbox("mdhd", 0, 0, mdhdPayload)

	handler := "vide"
	if tr.mediaType == "audio" {
		handler = "soun"
	}
	hdlrPayload := append(make([]byte, 4), []byte(handler)...) // pre_defined + handler_type
	hdlrPayload = append(hdlrPayload, make([]byte, 12)...)    // reserved
	hdlrPayload = append(hdlrPayload, 0)                      // empty name
	hdlr := fullbox("hdlr", 0, 0, hdlrPayload)

	mediaHeader := fullbox("vmhd", 0, 1, make([]byte, 8))
	if tr.mediaType == "audio" {
		mediaHeader = fullbox("smhd", 0, 0, make([]byte, 4))
	}
	urlBox := fullbox("url ", 0, 1, nil)
	dref := fullbox("dref", 0, 0, append(u32(1), urlBox...))
	dinf := box("dinf", dref)

	stbl := box("stbl", concat([][]byte{
		buildStsd(tr.mediaType),
		buildStts(tr.pkts),
		buildStss(tr),
		buildStsc(len(tr.pkts)),
		buildStsz(tr.pkts),
		box("stco", concat([][]byte{u32(0), u32(1), u32(uint32(tr.dataOff))})),
	}))
	minf := box("minf", concat([][]byte{mediaHeader, dinf, stbl}))
	mdia := box("mdia", concat([][]byte{mdhd, hdlr, minf}))
	return concat([][]byte{tkhd, mdia})
}

// buildStsd emits a minimal generic sample entry; structural parsers read
// the tables, not the codec description.
func buildStsd(mediaType string) []byte {
	typ := "mp4v"
	if mediaType == "audio" {
		typ = "mp4a"
	}
	entry := make([]byte, 8) // reserved(6) + data_reference_index(2)
	entry[7] = 1
	return fullbox("stsd", 0, 0, append(u32(1), box(typ, entry)...))
}

// buildStts compresses per-sample DTS deltas into runs (unified base).
// The final sample reuses the last delta so every sample has a duration.
func buildStts(pkts []scan.Packet) []byte {
	payload := u32(0) // version/flags; entry count appended below
	countPos := len(payload)
	payload = append(payload, u32(0)...)
	var count uint32
	var lastDelta int64
	for i := 1; i < len(pkts); i++ {
		delta := pkts[i].DTS - pkts[i-1].DTS
		if delta < 0 {
			delta = 0 // regressions already reported upstream
		}
		payload = append(payload, u32(1)...)
		payload = append(payload, u32(uint32(delta))...)
		count++
		lastDelta = delta
	}
	if len(pkts) > 0 {
		payload = append(payload, u32(1)...)
		payload = append(payload, u32(uint32(lastDelta))...)
		count++
	}
	be32(payload[countPos:countPos+4], count)
	return box("stts", payload)
}

func buildStss(tr *trackOut) []byte {
	if tr.mediaType != "video" {
		return nil
	}
	payload := concat([][]byte{u32(0), u32(0)}) // version/flags + count
	countPos := 4
	var count uint32
	for _, p := range tr.pkts {
		if p.Keyframe {
			payload = append(payload, u32(p.Index)...)
			count++
		}
	}
	be32(payload[countPos:countPos+4], count)
	return box("stss", payload)
}

// buildStsc places all samples of a track in one chunk.
func buildStsc(n int) []byte {
	// version/flags, entry_count=1, {first_chunk=1, samples_per_chunk=n, sdi=1}
	return box("stsc", concat([][]byte{u32(0), u32(1), u32(1), u32(uint32(n)), u32(1)}))
}

func buildStsz(pkts []scan.Packet) []byte {
	// version/flags, sample_size=0, sample_count, entries
	payload := concat([][]byte{u32(0), u32(0), u32(uint32(len(pkts)))})
	for _, p := range pkts {
		payload = append(payload, u32(p.Size)...)
	}
	return box("stsz", payload)
}

func writeMatrix(b []byte) {
	// Unity matrix: 0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000
	be32(b[0:4], 0x00010000)
	be32(b[12:16], 0x00010000)
	be32(b[24:28], 0x40000000)
}

func concat(parts [][]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
