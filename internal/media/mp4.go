// Package media builds and reads the local MP4 archive masters used by the
// recovery pipeline. Fixtures are composed box by box so tests exercise the
// exact sample tables an operator would find on a restored tape, while parsing
// goes through go-mp4 so real archives are read with the same code.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// Limits for archive masters read from disk or generated in memory.
const (
	MaxTracks    = 64
	MaxSamples   = 5_000_000
	MaxFileBytes = 1 << 30
)

// SampleSpec is one sample table row.
type SampleSpec struct {
	Duration uint32 // in the track timescale
	Size     uint32
	Sync     bool
}

// TrackSpec describes one track to write.
type TrackSpec struct {
	ID        uint32
	Kind      string // "video" or "audio"
	Timescale uint32
	Codec     string // "h264", "h265", "av1", "aac"
	Width     uint16
	Height    uint16
	Channels  uint16
	// Payload is the track's chunk data, written contiguously to mdat.
	Payload []byte
	Samples []SampleSpec
}

// BuildMP4 writes an ISO BMFF file with exactly one chunk and one sample
// description per track. The output is fully deterministic.
func BuildMP4(tracks []TrackSpec) ([]byte, error) {
	if len(tracks) == 0 {
		return nil, errors.New("no tracks")
	}
	if len(tracks) > MaxTracks {
		return nil, fmt.Errorf("%d tracks exceeds limit %d", len(tracks), MaxTracks)
	}
	for _, t := range tracks {
		if t.Timescale == 0 {
			return nil, fmt.Errorf("track %d: zero timescale", t.ID)
		}
		if len(t.Samples) == 0 {
			return nil, fmt.Errorf("track %d: no samples", t.ID)
		}
		if len(t.Samples) > MaxSamples {
			return nil, fmt.Errorf("track %d: %d samples exceeds limit", t.ID, len(t.Samples))
		}
		var sum uint64
		for _, s := range t.Samples {
			if s.Size == 0 {
				return nil, fmt.Errorf("track %d: zero-size sample", t.ID)
			}
			sum += uint64(s.Size)
		}
		if sum != uint64(len(t.Payload)) {
			return nil, fmt.Errorf("track %d: sample sizes sum to %d, payload is %d bytes", t.ID, sum, len(t.Payload))
		}
	}

	ftyp := boxBytes("ftyp", append(append([]byte("isom"), u32(512)...), []byte("isomiso2avc1mp41")...))

	// First pass with zero offsets to measure moov, then patch the real chunk
	// offsets. All offset fields are fixed-width, so the size is stable.
	offsets := make([]uint32, len(tracks))
	moov := moovBytes(tracks, offsets)
	mdatPayloadStart := uint64(len(ftyp)) + uint64(len(moov)) + 8
	var cursor uint64
	for i := range tracks {
		if mdatPayloadStart+cursor > 0xffffffff {
			return nil, errors.New("media payload does not fit 32-bit chunk offsets")
		}
		offsets[i] = uint32(mdatPayloadStart + cursor)
		cursor += uint64(len(tracks[i].Payload))
	}
	moov2 := moovBytes(tracks, offsets)
	if len(moov2) != len(moov) {
		return nil, errors.New("internal: moov size changed while patching offsets")
	}
	moov = moov2

	var payload []byte
	for _, t := range tracks {
		payload = append(payload, t.Payload...)
	}
	mdat := boxBytes("mdat", payload)
	out := make([]byte, 0, len(ftyp)+len(moov)+len(mdat))
	out = append(out, ftyp...)
	out = append(out, moov...)
	out = append(out, mdat...)
	if uint64(len(out)) > MaxFileBytes {
		return nil, fmt.Errorf("output %d bytes exceeds limit %d", len(out), MaxFileBytes)
	}
	return out, nil
}

func trackDurationTicks(t TrackSpec) uint64 {
	var d uint64
	for _, s := range t.Samples {
		d += uint64(s.Duration)
	}
	return d
}

func moovBytes(tracks []TrackSpec, offsets []uint32) []byte {
	var maxMovieDur uint64
	for _, t := range tracks {
		d := trackDurationTicks(t) * 1000 / uint64(t.Timescale)
		if d > maxMovieDur {
			maxMovieDur = d
		}
	}
	body := mvhdBytes(uint32(maxMovieDur), uint32(len(tracks)+1))
	for i, t := range tracks {
		body = append(body, trakBytes(t, offsets[i])...)
	}
	return boxBytes("moov", body)
}

func trakBytes(t TrackSpec, chunkOffset uint32) []byte {
	movieDur := uint32(trackDurationTicks(t) * 1000 / uint64(t.Timescale))
	body := tkhdBytes(t, movieDur)
	body = append(body, mdiaBytes(t, chunkOffset)...)
	return boxBytes("trak", body)
}

func tkhdBytes(t TrackSpec, dur uint32) []byte {
	b := u32(0, 0, t.ID, 0, dur, 0, 0)
	b = append(b, u16(0, 0, 0, 0)...)
	width := uint32(t.Width) << 16
	height := uint32(t.Height) << 16
	if t.Kind == "audio" {
		width, height = 0, 0
	}
	b = append(b, u32(0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000, width, height)...)
	return fullBox("tkhd", 0, 7, b) // enabled | in movie | in preview
}

func mdiaBytes(t TrackSpec, chunkOffset uint32) []byte {
	mdhdBody := u32(0, 0, t.Timescale, uint32(trackDurationTicks(t)))
	mdhdBody = append(mdhdBody, u16(0x55c4, 0)...)
	mdhd := fullBox("mdhd", 0, 0, mdhdBody)

	handler := []byte("vide")
	name := "VideoHandler"
	if t.Kind == "audio" {
		handler = []byte("soun")
		name = "SoundHandler"
	}
	hdlrBody := append(append(u32(0), handler...), u32(0, 0, 0)...)
	hdlrBody = append(hdlrBody, []byte(name)...)
	hdlrBody = append(hdlrBody, 0)
	hdlr := fullBox("hdlr", 0, 0, hdlrBody)

	body := append(append(mdhd, hdlr...), minfBytes(t, chunkOffset)...)
	return boxBytes("mdia", body)
}

func minfBytes(t TrackSpec, chunkOffset uint32) []byte {
	var head []byte
	if t.Kind == "audio" {
		head = fullBox("smhd", 0, 0, u16(0, 0))
	} else {
		head = fullBox("vmhd", 0, 1, u16(0, 0, 0, 0))
	}
	url := fullBox("url ", 0, 1, nil)
	dref := fullBox("dref", 0, 0, append(u32(1), url...))
	body := append(append(head, boxBytes("dinf", dref)...), stblBytes(t, chunkOffset)...)
	return boxBytes("minf", body)
}

func stblBytes(t TrackSpec, chunkOffset uint32) []byte {
	sizes := make([]uint32, len(t.Samples))
	syncNumbers := make([]uint32, 0, len(t.Samples))
	for i, s := range t.Samples {
		sizes[i] = s.Size
		if s.Sync {
			syncNumbers = append(syncNumbers, uint32(i+1))
		}
	}
	var entry []byte
	if t.Kind == "audio" {
		entry = audioSampleEntry(t)
	} else {
		entry = videoSampleEntry(t)
	}
	body := fullBox("stsd", 0, 0, append(u32(1), entry...))
	body = append(body, sttsBytes(t)...)
	body = append(body, fullBox("stsc", 0, 0, append(u32(1), u32(1, uint32(len(t.Samples)), 1)...))...)
	body = append(body, fullBox("stsz", 0, 0, append(u32(0, uint32(len(sizes))), u32(sizes...)...))...)
	body = append(body, fullBox("stco", 0, 0, append(u32(1), u32(chunkOffset)...))...)
	if t.Kind == "video" {
		body = append(body, fullBox("stss", 0, 0, append(u32(uint32(len(syncNumbers))), u32(syncNumbers...)...))...)
	}
	return boxBytes("stbl", body)
}

// sttsBytes run-length encodes contiguous equal sample durations.
func sttsBytes(t TrackSpec) []byte {
	type run struct{ count, delta uint32 }
	var runs []run
	for _, s := range t.Samples {
		if n := len(runs); n > 0 && runs[n-1].delta == s.Duration {
			runs[n-1].count++
			continue
		}
		runs = append(runs, run{1, s.Duration})
	}
	b := u32(uint32(len(runs)))
	for _, r := range runs {
		b = append(b, u32(r.count, r.delta)...)
	}
	return fullBox("stts", 0, 0, b)
}

func videoSampleEntry(t TrackSpec) []byte {
	b := make([]byte, 6)
	b = append(b, u16(1)...) // data reference index
	b = append(b, u16(0, 0)...)
	b = append(b, u32(0, 0, 0)...)
	b = append(b, u16(t.Width, t.Height)...)
	b = append(b, u32(0x00480000, 0x00480000, 0)...)
	b = append(b, u16(1)...)
	b = append(b, make([]byte, 32)...)
	b = append(b, u16(0x18, 0xffff)...)
	var cfg []byte
	switch t.Codec {
	case "h265":
		cfg = boxBytes("hvcC", hvcCBody())
	case "av1":
		cfg = boxBytes("av1C", av1CBody())
	default:
		cfg = boxBytes("avcC", avcCBody())
	}
	return boxBytes(sampleEntryType(t.Codec), append(b, cfg...))
}

func sampleEntryType(codec string) string {
	switch codec {
	case "h265":
		return "hvc1"
	case "av1":
		return "av01"
	default:
		return "avc1"
	}
}

func audioSampleEntry(t TrackSpec) []byte {
	b := make([]byte, 6)
	b = append(b, u16(1)...)
	b = append(b, u16(0, 0)...)
	b = append(b, u16(0, 0)...)
	ch := t.Channels
	if ch == 0 {
		ch = 2
	}
	b = append(b, u16(ch, 16, 0, 0)...)
	b = append(b, u32(uint32(t.Timescale)<<16)...)
	return boxBytes("mp4a", b)
}

func avcCBody() []byte {
	return []byte{1, 0x64, 0, 0x1f, 0xff, 0xe1, 0, 4, 0x67, 0x64, 0, 0x1f, 1, 0, 4, 0x68, 0xee, 0x3c, 0x80}
}

func hvcCBody() []byte {
	return []byte{1, 0x01, 0x60, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x5d, 0xf0, 0x00, 0xfc, 0xfd, 0xf8, 0xf8, 0x00, 0x00, 0x0f, 0x03}
}

func av1CBody() []byte {
	return []byte{0x81, 0x00, 0x0c, 0x00}
}

func mvhdBytes(duration, nextTrack uint32) []byte {
	b := u32(0, 0, 1000, duration)
	b = append(b, u32(0x10000)...)
	b = append(b, u16(0x100, 0)...)
	b = append(b, u32(0, 0)...)
	b = append(b, u32(0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000)...)
	b = append(b, make([]byte, 24)...)
	b = append(b, u32(nextTrack)...)
	return fullBox("mvhd", 0, 0, b)
}

// ---- box primitives ----

func boxBytes(typ string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], typ)
	return append(out, body...)
}

func fullBox(typ string, version byte, flags uint32, body []byte) []byte {
	b := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return boxBytes(typ, append(b, body...))
}

func u32(v ...uint32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint32(b[i*4:], x)
	}
	return b
}

func u16(v ...uint16) []byte {
	b := make([]byte, 2*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint16(b[i*2:], x)
	}
	return b
}

// ---- parsing ----

// ParsedTrack is a track read back from an archive master.
type ParsedTrack struct {
	ID        uint32
	Kind      string
	Timescale uint32
	Codec     string
	Durations []uint32
	Sizes     []uint32
	Sync      []bool
	PTS       []int64 // decode time in integer nanoseconds
	Keyframes []int   // indexes into PTS
}

// ParsedFile is a probed archive master.
type ParsedFile struct {
	Tracks    []ParsedTrack
	Timescale uint32
	Duration  uint64
	// Data is the first mdat payload, used by the stream scanner.
	Data []byte
	// ChunkOffset maps track ID to its first chunk's absolute file offset.
	ChunkOffset map[uint32]int64
}

// ParseFile reads an archive master from r. size bounds the read.
func ParseFile(r io.ReadSeeker, size int64) (*ParsedFile, error) {
	if size > MaxFileBytes {
		return nil, fmt.Errorf("file size %d exceeds limit %d", size, MaxFileBytes)
	}
	info, err := mp4.Probe(r)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	if len(info.Tracks) == 0 {
		return nil, errors.New("archive master has no tracks")
	}
	if len(info.Tracks) > MaxTracks {
		return nil, fmt.Errorf("%d tracks exceeds limit %d", len(info.Tracks), MaxTracks)
	}
	pf := &ParsedFile{Timescale: info.Timescale, Duration: info.Duration, ChunkOffset: map[uint32]int64{}}
	for _, tr := range info.Tracks {
		if len(tr.Samples) > MaxSamples {
			return nil, fmt.Errorf("track %d: %d samples exceeds limit", tr.TrackID, len(tr.Samples))
		}
		pt := ParsedTrack{ID: tr.TrackID, Timescale: tr.Timescale, Kind: trackKind(tr)}
		var ticks uint64
		scale := uint64(tr.Timescale)
		if scale == 0 {
			scale = 1
		}
		for _, s := range tr.Samples {
			pt.Durations = append(pt.Durations, s.TimeDelta)
			pt.Sizes = append(pt.Sizes, s.Size)
			pt.PTS = append(pt.PTS, clockToNs(ticks, scale))
			ticks += uint64(s.TimeDelta)
		}
		pt.Sync = make([]bool, len(pt.PTS))
		for i := range pt.Sync {
			pt.Sync[i] = true
			pt.Keyframes = append(pt.Keyframes, i)
		}
		if len(tr.Chunks) > 0 {
			pf.ChunkOffset[tr.TrackID] = int64(tr.Chunks[0].DataOffset)
		}
		pf.Tracks = append(pf.Tracks, pt)
	}
	if err := readStss(r, pf); err != nil {
		return nil, err
	}
	if err := readMdat(r, pf); err != nil {
		return nil, err
	}
	return pf, nil
}

// clockToNs converts ticks to ns with saturation at one century.
func clockToNs(ticks, scale uint64) int64 {
	const century = uint64(3_155_760_000_000_000_000)
	const ns = uint64(1_000_000_000)
	whole := ticks / scale
	if whole > century/ns {
		return int64(century)
	}
	rem := ticks % scale
	return int64(whole*ns + (rem*ns+scale/2)/scale)
}

func trackKind(tr *mp4.Track) string {
	switch tr.Codec {
	case mp4.CodecAVC1:
		return "video"
	case mp4.CodecMP4A:
		return "audio"
	default:
		return "unknown"
	}
}

// readStss fills each video track's keyframe flags from its stss box. Tracks
// without stss stay all-sync, matching ISO BMFF semantics.
func readStss(r io.ReadSeeker, pf *ParsedFile) error {
	traks, err := mp4.ExtractBoxes(r, nil, []mp4.BoxPath{
		{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()},
	})
	if err != nil {
		return fmt.Errorf("extract trak: %w", err)
	}
	if len(traks) != len(pf.Tracks) {
		return fmt.Errorf("found %d trak boxes for %d probed tracks", len(traks), len(pf.Tracks))
	}
	for i, trak := range traks {
		bis, err := mp4.ExtractBoxes(r, trak, []mp4.BoxPath{
			{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss()},
		})
		if err != nil {
			return fmt.Errorf("extract stss: %w", err)
		}
		if len(bis) == 0 {
			continue
		}
		bi := bis[0]
		if _, err := bi.SeekToPayload(r); err != nil {
			return err
		}
		var stss mp4.Stss
		if _, err := mp4.Unmarshal(r, bi.Size-bi.HeaderSize, &stss, bi.Context); err != nil {
			return fmt.Errorf("unmarshal stss: %w", err)
		}
		pt := &pf.Tracks[i]
		for j := range pt.Sync {
			pt.Sync[j] = false
		}
		pt.Keyframes = pt.Keyframes[:0]
		for _, n := range stss.SampleNumber {
			if n >= 1 && int(n) <= len(pt.Sync) {
				pt.Sync[n-1] = true
				pt.Keyframes = append(pt.Keyframes, int(n-1))
			}
		}
	}
	return nil
}

// readMdat reads the first mdat payload into memory for scanning.
func readMdat(r io.ReadSeeker, pf *ParsedFile) error {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for {
		bi, err := mp4.ReadBoxInfo(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if bi.Type == mp4.BoxTypeMdat() {
			size := bi.Size - bi.HeaderSize
			if size > MaxFileBytes {
				return fmt.Errorf("mdat size %d exceeds limit", size)
			}
			payload := make([]byte, size)
			if _, err := bi.SeekToPayload(r); err != nil {
				return err
			}
			if _, err := io.ReadFull(r, payload); err != nil {
				return fmt.Errorf("read mdat: %w", err)
			}
			pf.Data = payload
			return nil
		}
		if _, err := bi.SeekToEnd(r); err != nil {
			return err
		}
	}
}

// Reader returns a fresh random-access reader over an in-memory master.
func Reader(b []byte) io.ReadSeeker { return bytes.NewReader(b) }
