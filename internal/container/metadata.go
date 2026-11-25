package container

import (
	"encoding/binary"
	"fmt"

	"archivectl/internal/timeline"
)

// KindFourCC maps stable track kinds to compact MP4 handler-style codes.
func KindFourCC(kind string) string {
	switch kind {
	case KindVideo:
		return "vide"
	case KindAudio:
		return "soun"
	case KindSubtitle:
		return "sbtl"
	case KindChapters:
		return "chap"
	case KindMetadata:
		return "meta"
	default:
		return "soun"
	}
}

func kindFromFourCC(four string) string {
	switch four {
	case "vide":
		return KindVideo
	case "soun":
		return KindAudio
	case "sbtl", "text", "subt":
		return KindSubtitle
	case "chap", "chpl":
		return KindChapters
	case "meta", "id3 ", "ID32":
		return KindMetadata
	default:
		return KindMetadata
	}
}

// EncodeTrackIndex builds the bounded archive sample-index box ("aidx").
// Offsets are absolute in the destination/source file and are checked when the
// box is parsed.
func EncodeTrackIndex(t Track) []byte {
	sampleCount := len(t.Samples)
	size := 4 + 4 + 4 + 4 + 8 + 8 + 4 + 4*len(t.References) + 4 + sampleCount*40
	b := make([]byte, size)
	o := 0
	put32 := func(v uint32) { binary.BigEndian.PutUint32(b[o:o+4], v); o += 4 }
	put64 := func(v int64) { binary.BigEndian.PutUint64(b[o:o+8], uint64(v)); o += 8 }
	put32(t.ID)
	put32(t.Timescale)
	copy(b[o:o+4], KindFourCC(t.Kind))
	o += 4
	codec := t.Codec
	if codec == "" {
		codec = "bin"
	}
	var cc [4]byte
	copy(cc[:], codec)
	copy(b[o:o+4], cc[:])
	o += 4
	put64(t.Origin)
	put64(t.Duration)
	put32(uint32(len(t.References)))
	for _, r := range t.References {
		put32(r)
	}
	put32(uint32(sampleCount))
	for _, s := range t.Samples {
		put64(s.DTS)
		put64(s.PTS)
		if s.Duration < 0 {
			s.Duration = 0 // encoding a validated asset never emits negative durations
		}
		put64(s.Duration)
		put32(s.Flags)
		put64(s.FileOffset)
		put32(uint32(s.Size))
	}
	return BuildBox("aidx", b)
}

// ParseTrackIndex parses one aidx payload. boxStart is the absolute offset of
// the enclosing box and is used to locate invalid duration fields.
func ParseTrackIndex(payload []byte, boxStart int64) (Track, error) {
	var t Track
	if len(payload) < 36 {
		return t, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "parse aidx", Offset: boxStart, TrackID: t.ID, Detail: "payload too short"}
	}
	o := 0
	get32 := func() uint32 { v := binary.BigEndian.Uint32(payload[o : o+4]); o += 4; return v }
	get64 := func() int64 { v := int64(binary.BigEndian.Uint64(payload[o : o+8])); o += 8; return v }
	t.ID = get32()
	t.Timescale = get32()
	t.Kind = kindFromFourCC(fourCC(payload[o : o+4]))
	o += 4
	t.Codec = fourCC(payload[o : o+4])
	o += 4
	t.Origin = get64()
	t.Duration = get64()
	refCount := int(get32())
	if refCount > 1024 || o+4*refCount+4 > len(payload) {
		return t, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "parse aidx references", Offset: boxStart + int64(o), TrackID: t.ID, Detail: "invalid reference count"}
	}
	t.References = make([]uint32, refCount)
	for i := range t.References {
		t.References[i] = get32()
	}
	n := int(get32())
	if n > 1_000_000 || o+n*40 > len(payload) {
		return t, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "parse aidx samples", Offset: boxStart + int64(o), TrackID: t.ID, Detail: fmt.Sprintf("sample count %d exceeds payload", n)}
	}
	t.Samples = make([]Sample, n)
	tb := timeline.Timebase{Timescale: t.Timescale, Start: t.Origin}
	for i := 0; i < n; i++ {
		fieldOffset := boxStart + 8 + int64(o+16) // duration is third int64 in record
		dts := get64()
		pts := get64()
		dur := get64()
		if dur < 0 {
			return t, &timeline.Error{Code: timeline.CodeNegativeDuration, Op: "parse sample duration", Offset: fieldOffset, TrackID: t.ID, Detail: fmt.Sprintf("sample %d duration=%d", i, dur)}
		}
		flags := get32()
		dataOffset := get64()
		size := int(get32())
		ns, err := tb.Normalize(pts)
		if err != nil {
			return t, err
		}
		t.Samples[i] = Sample{
			Index: i, DTS: dts, PTS: pts, Duration: dur, Flags: flags,
			FileOffset: dataOffset, Size: size, TimeNS: ns,
		}
	}
	if err := timeline.CheckDTSMonotonic(t.ID, dtsValues(t.Samples)); err != nil {
		return t, err
	}
	return t, nil
}

func dtsValues(s []Sample) []int64 {
	out := make([]int64, len(s))
	for i := range s {
		out[i] = s[i].DTS
	}
	return out
}

// BuildEMSG builds a version-0 event message full box.
func BuildEMSG(scheme, value string, timescale uint32, presentationDelta uint32, duration uint32, id uint32, message []byte) []byte {
	p := make([]byte, 0, len(scheme)+len(value)+len(message)+24)
	p = append(p, 0) // version
	p = append(p, 0, 0, 0)
	p = append(p, []byte(scheme)...)
	p = append(p, 0)
	p = append(p, []byte(value)...)
	p = append(p, 0)
	var num [4]byte
	binary.BigEndian.PutUint32(num[:], timescale)
	p = append(p, num[:]...)
	binary.BigEndian.PutUint32(num[:], presentationDelta)
	p = append(p, num[:]...)
	binary.BigEndian.PutUint32(num[:], duration)
	p = append(p, num[:]...)
	binary.BigEndian.PutUint32(num[:], id)
	p = append(p, num[:]...)
	p = append(p, message...)
	return BuildBox("emsg", p)
}

// ParseEMSG parses the constrained version-0 emsg produced by the fixtures.
func ParseEMSG(payload []byte, boxStart int64) (Event, error) {
	e := Event{Kind: EventEMSG, ByteOffset: boxStart}
	if len(payload) < 20 || payload[0] != 0 {
		return e, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "parse emsg", Offset: boxStart, Detail: "unsupported or truncated emsg"}
	}
	p := payload[4:]
	readCString := func() (string, error) {
		i := indexByte(p, 0)
		if i < 0 {
			return "", &timeline.Error{Code: timeline.CodeCorruptBox, Op: "parse emsg string", Offset: boxStart}
		}
		s := string(p[:i])
		p = p[i+1:]
		return s, nil
	}
	scheme, err := readCString()
	if err != nil {
		return e, err
	}
	value, err := readCString()
	if err != nil {
		return e, err
	}
	if len(p) < 16 {
		return e, &timeline.Error{Code: timeline.CodeCorruptBox, Op: "parse emsg times", Offset: boxStart}
	}
	ts := binary.BigEndian.Uint32(p[0:4])
	delta := binary.BigEndian.Uint32(p[4:8])
	_ = binary.BigEndian.Uint32(p[8:12])
	id := binary.BigEndian.Uint32(p[12:16])
	msg := append([]byte(nil), p[16:]...)
	e.Timescale = ts
	e.RawTime = int64(delta)
	e.Label = fmt.Sprintf("%s/%s/id=%d", scheme, value, id)
	e.Payload = msg
	e.Hash = HashBytes(msg)
	e.TimeNS, err = timeline.ToNanoseconds(int64(delta), ts)
	if err != nil {
		return e, err
	}
	return e, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
